package internal

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/soheilhy/cmux"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpc"
	"github.com/determined-ai/determined/master/internal/oauth"
	"github.com/determined-ai/determined/master/internal/proxy"
	"github.com/determined-ai/determined/master/internal/resourcemanagers"
	"github.com/determined-ai/determined/master/internal/saml"
	"github.com/determined-ai/determined/master/internal/scim"
	"github.com/determined-ai/determined/master/internal/telemetry"
	"github.com/determined-ai/determined/master/internal/template"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/actor"
	"github.com/determined-ai/determined/master/pkg/actor/actors"
	aproto "github.com/determined-ai/determined/master/pkg/agent"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

const (
	defaultAskTimeout = 2 * time.Second
	webuiBaseRoute    = "/det"
)

// Master manages the Determined master state.
type Master struct {
	ClusterID string
	MasterID  string
	Version   string

	config   *Config
	taskSpec *tasks.TaskSpec

	logs          *logger.LogBuffer
	system        *actor.System
	echo          *echo.Echo
	rm            *actor.Ref
	rwCoordinator *actor.Ref
	db            *db.PgDB
	proxy         *actor.Ref
	trialLogger   *actor.Ref
}

// New creates an instance of the Determined master.
func New(version string, logStore *logger.LogBuffer, config *Config) *Master {
	logger.SetLogrus(config.Log)
	return &Master{
		MasterID: uuid.New().String(),
		Version:  version,
		logs:     logStore,
		config:   config,
	}
}

func (m *Master) getConfig(c echo.Context) (interface{}, error) {
	return m.config.Printable()
}

func (m *Master) getInfo(c echo.Context) (interface{}, error) {
	telemetryInfo := aproto.TelemetryInfo{}

	if m.config.Telemetry.Enabled && m.config.Telemetry.SegmentWebUIKey != "" {
		// Only advertise a Segment WebUI key if a key has been configured and
		// telemetry is enabled.
		telemetryInfo.Enabled = true
		telemetryInfo.SegmentKey = m.config.Telemetry.SegmentWebUIKey
	}

	ssoProviderInfo := make([]aproto.SSOProviderInfo, 0)
	if m.config.SAML.Enabled {
		u, err := url.Parse(m.config.SAML.IDPRecipientURL)
		if err != nil {
			return nil, errors.Wrap(err, "error parsing SAML recipient URL")
		}
		u.Path = saml.SAMLRoot + saml.InitiatePath
		ssoProviderInfo = append(ssoProviderInfo, aproto.SSOProviderInfo{
			SSOInitiateURL: u.String(),
			Name:           m.config.SAML.Provider,
		})
	}

	return &aproto.MasterInfo{
		ClusterID:    m.ClusterID,
		MasterID:     m.MasterID,
		Version:      m.Version,
		Telemetry:    telemetryInfo,
		ClusterName:  m.config.ClusterName,
		SSOProviders: ssoProviderInfo,
	}, nil
}

func (m *Master) getMasterLogs(c echo.Context) (interface{}, error) {
	args := struct {
		LessThanID    *int `query:"less_than_id"`
		GreaterThanID *int `query:"greater_than_id"`
		Limit         *int `query:"tail"`
	}{}
	if err := api.BindArgs(&args, c); err != nil {
		return nil, err
	}

	limit := -1
	if args.Limit != nil {
		limit = *args.Limit
	}

	startID := -1
	if args.GreaterThanID != nil {
		startID = *args.GreaterThanID + 1
	}

	endID := -1
	if args.LessThanID != nil {
		endID = *args.LessThanID
	}

	entries := m.logs.Entries(startID, endID, limit)
	if len(entries) == 0 {
		// Return a zero-length array here so the JSON encoding is `[]` rather than `null`.
		entries = make([]*logger.Entry, 0)
	}
	return entries, nil
}

func (m *Master) startServers(cert *tls.Certificate) error {
	// Create the base TCP socket listener and, if configured, set up TLS wrapping.
	baseListener, err := net.Listen("tcp", fmt.Sprintf(":%d", m.config.Port))
	if err != nil {
		return err
	}

	if cert != nil {
		baseListener = tls.NewListener(baseListener, &tls.Config{
			Certificates:             []tls.Certificate{*cert},
			MinVersion:               tls.VersionTLS12,
			PreferServerCipherSuites: true,
		})
	}

	// Initialize listeners and multiplexing.
	if err := grpc.RegisterHTTPProxy(m.echo, m.config.Port, cert); err != nil {
		return errors.Wrap(err, "failed to register gRPC gateway")
	}

	mux := cmux.New(baseListener)
	grpcListener := mux.MatchWithWriters(
		cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"),
	)
	httpListener := mux.Match(cmux.HTTP1(), cmux.HTTP2())

	// Start all servers and return the first error. This leaks a channel, but the complexity of
	// perfectly handling cleanup and all the error cases doesn't seem worth it for a function that is
	// called exactly once and causes the whole process to exit immediately when it returns.
	errs := make(chan error)
	start := func(name string, run func() error) {
		go func() {
			errs <- errors.Wrap(run(), name+" failed")
		}()
	}
	start("gRPC server", func() error {
		return grpc.NewGRPCServer(m.db, &apiServer{m: m}).Serve(grpcListener)
	})
	start("HTTP server", func() error {
		m.echo.Listener = httpListener
		m.echo.HidePort = true
		return m.echo.StartServer(m.echo.Server)
	})
	start("cmux listener", mux.Serve)

	log.Infof("accepting incoming connections on port %d", m.config.Port)
	return <-errs
}

func (m *Master) restoreExperiment(e *model.Experiment) {
	// Check if the returned config is the zero value, i.e. the config could not be parsed
	// correctly. If the config could not be parsed, mark the experiment as errored.
	if !reflect.DeepEqual(e.Config, model.ExperimentConfig{}) {
		err := restoreExperiment(m, e)
		if err == nil {
			return
		}
		log.WithError(err).Errorf("failed to restore experiment: %d", e.ID)
	} else {
		log.Errorf("failed to parse experiment config: %d", e.ID)
	}
	e.State = model.ErrorState
	if err := m.db.TerminateExperimentInRestart(e.ID, e.State); err != nil {
		log.WithError(err).Error("failed to mark experiment as errored")
	}
	telemetry.ReportExperimentStateChanged(m.system, m.db, *e)
}

// convertDBErrorsToNotFound helps reduce boilerplate in our handlers, by
// classifying database "not found" errors as HTTP "not found" errors.
func convertDBErrorsToNotFound(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := next(c)
		if errors.Cause(err) == db.ErrNotFound {
			return echo.ErrNotFound
		}
		return err
	}
}

func getMasterURL(config *Config) (*url.URL, error) {
	// DET-2035: move master URL field out of provisioner and avoid brittle
	// inference of the master URL.
	var s string
	if p := config.Provisioner; p == nil {
		s = "http://localhost:8080"
	} else {
		s = p.MasterURL
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return u, nil
}

func (m *Master) rwCoordinatorWebSocket(socket *websocket.Conn, c echo.Context) error {
	c.Logger().Infof(
		"New connection for RW Coordinator from: %v, %s",
		socket.RemoteAddr(),
		c.Request().URL,
	)

	resourceName := c.Request().URL.Path
	query := c.Request().URL.Query()

	readLockString, ok := query["read_lock"]
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("Received request without specifying read_lock: %v", c.Request().URL))
	}

	var readLock bool
	if strings.EqualFold(readLockString[0], "True") {
		readLock = true
	} else {
		if !strings.EqualFold(readLockString[0], "false") {
			return echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("Received request with invalid read_lock: %v", c.Request().URL))
		}
		readLock = false
	}

	socketActor := m.system.AskAt(actor.Addr("rwCoordinator"),
		resourceRequest{resourceName, readLock, socket})
	actorRef, ok := socketActor.Get().(*actor.Ref)
	if !ok {
		c.Logger().Errorf("Failed to get websocket actor")
		return nil
	}

	// Wait for the websocket actor to terminate.
	return actorRef.AwaitTermination()
}

func (m *Master) postTrialLogs(c echo.Context) (interface{}, error) {
	body, err := ioutil.ReadAll(c.Request().Body)
	if err != nil {
		return nil, err
	}

	var logs []model.TrialLog
	if err = json.Unmarshal(body, &logs); err != nil {
		return nil, err
	}

	for _, l := range logs {
		if l.TrialID == 0 {
			continue
		}
		m.system.Tell(m.trialLogger, l)
	}
	return "", nil
}

// Run causes the Determined master to connect the database and begin listening for HTTP requests.
func (m *Master) Run() error {
	log.Infof("Determined master %s (built with %s)", m.Version, runtime.Version())

	var err error

	if err = etc.SetRootPath(filepath.Join(m.config.Root, "static/srv")); err != nil {
		return errors.Wrap(err, "could not set static root")
	}

	m.db, err = db.Setup(&m.config.DB)
	if err != nil {
		return err
	}

	m.ClusterID, err = m.db.GetClusterID()
	if err != nil {
		return errors.Wrap(err, "could not fetch cluster id from database")
	}
	cert, err := m.config.Security.TLS.ReadCertificate()
	if err != nil {
		return errors.Wrap(err, "failed to read TLS certificate")
	}
	m.taskSpec = &tasks.TaskSpec{
		ClusterID:             m.ClusterID,
		HarnessPath:           filepath.Join(m.config.Root, "wheels"),
		TaskContainerDefaults: m.config.TaskContainerDefaults,
		MasterCert:            cert,
	}

	go m.cleanUpSearcherEvents()

	// Actor structure:
	// master system
	// +- Agent Group (actors.Group: agents)
	//     +- Agent (internal.agent: <agent-id>)
	//         +- Websocket (actors.WebSocket: <remote-address>)
	// +- ResourceManagers (scheduler.ResourceManagers: resourceManagers)
	// Exactly one of the resource managers is enabled at a time.
	// +- AgentResourceManager (resourcemanagers.AgentResourceManager: agentRM)
	//     +- Resource Pool (resourcemanagers.ResourcePool: <resource-pool-name>)
	//         +- Provisioner (provisioner.Provisioner: provisioner)
	// +- KubernetesResourceManager (scheduler.KubernetesResourceManager: kubernetesRM)
	// +- Service Proxy (proxy.Proxy: proxy)
	// +- RWCoordinator (internal.rw_coordinator: rwCoordinator)
	// +- Telemetry (telemetry.telemetryActor: telemetry)
	// +- TrialLogger (internal.trialLogger: trialLogger)
	// +- Experiments (actors.Group: experiments)
	//     +- Experiment (internal.experiment: <experiment-id>)
	//         +- Trial (internal.trial: <trial-request-id>)
	//             +- Websocket (actors.WebSocket: <remote-address>)
	m.system = actor.NewSystem("master")

	m.trialLogger, _ = m.system.ActorOf(actor.Addr("trialLogger"), newTrialLogger(m.db))

	userService, err := user.New(m.db, m.system)
	if err != nil {
		return errors.Wrap(err, "cannot initialize user manager")
	}
	authFuncs := []echo.MiddlewareFunc{userService.ProcessAuthentication}

	m.proxy, _ = m.system.ActorOf(actor.Addr("proxy"), &proxy.Proxy{})

	// Used to decide whether we add trailing slash to the paths or not affecting
	// relative links in web pages hosted under these routes.
	staticWebDirectoryPaths := map[string]bool{
		"/docs":          true,
		webuiBaseRoute:   true,
		"/docs/rest-api": true,
	}

	// Initialize the HTTP server and listen for incoming requests.
	m.echo = echo.New()
	m.echo.Use(middleware.Recover())
	m.echo.Use(middleware.AddTrailingSlashWithConfig(middleware.TrailingSlashConfig{
		Skipper: func(c echo.Context) bool {
			return !staticWebDirectoryPaths[c.Path()]
		},
		RedirectCode: http.StatusMovedPermanently,
	}))
	setupEchoRedirects(m)

	if m.config.EnableCors {
		m.echo.Use(api.CORSWithTargetedOrigin)
	}

	// Add resistance to common HTTP attacks.
	//
	// TODO(DET-1696): Enable Content Security Policy (CSP).
	secureConfig := middleware.SecureConfig{
		Skipper:            middleware.DefaultSkipper,
		XSSProtection:      "1; mode=block",
		ContentTypeNosniff: "nosniff",
		XFrameOptions:      "SAMEORIGIN",
	}
	m.echo.Use(middleware.SecureWithConfig(secureConfig))

	// Register middleware that extends default context.
	m.echo.Use(func(h echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cc := &context.DetContext{Context: c}
			return h(cc)
		}
	})

	m.echo.Use(convertDBErrorsToNotFound)

	m.echo.Logger = logger.New()
	m.echo.HideBanner = true
	m.echo.HTTPErrorHandler = api.JSONErrorHandler

	// Resource Manager.
	m.rm = resourcemanagers.Setup(
		m.system, m.echo, m.config.ResourceManager, m.config.ResourcePoolsConfig, cert,
	)
	tasksGroup := m.echo.Group("/tasks", authFuncs...)
	tasksGroup.GET("", api.Route(m.getTasks))
	tasksGroup.GET("/:task_id", api.Route(m.getTask))

	// Distributed lock server.
	rwCoordinator := newRWCoordinator()
	m.rwCoordinator, _ = m.system.ActorOf(actor.Addr("rwCoordinator"), rwCoordinator)

	// Restore non-terminal experiments from the database.
	m.system.ActorOf(actor.Addr("experiments"), &actors.Group{})
	toRestore, err := m.db.NonTerminalExperiments()
	if err != nil {
		return errors.Wrap(err, "couldn't retrieve experiments to restore")
	}
	for _, exp := range toRestore {
		go m.restoreExperiment(exp)
	}

	// Docs and WebUI.
	webuiRoot := filepath.Join(m.config.Root, "webui")
	reactRoot := filepath.Join(webuiRoot, "react")
	reactRootAbs, err := filepath.Abs(reactRoot)
	if err != nil {
		return errors.Wrap(err, "failed to get absolute path to react root")
	}
	reactIndex := filepath.Join(reactRoot, "index.html")

	// Docs.
	m.echo.Static("/docs/rest-api", filepath.Join(webuiRoot, "docs", "rest-api"))
	m.echo.Static("/docs", filepath.Join(webuiRoot, "docs"))

	webuiGroup := m.echo.Group(webuiBaseRoute)
	webuiGroup.File("/", reactIndex)
	webuiGroup.GET("/*", func(c echo.Context) error {
		groupPath := strings.TrimPrefix(c.Request().URL.Path, webuiBaseRoute+"/")
		requestedFile := filepath.Join(reactRoot, groupPath)
		// We do a simple check against directory traversal attacks.
		requestedFileAbs, err := filepath.Abs(requestedFile)
		if err != nil {
			log.WithError(err).Error("failed to get absolute path to requested file")
			return c.File(reactIndex)
		}
		isInReactDir := strings.HasPrefix(requestedFileAbs, reactRootAbs)
		if !isInReactDir {
			return echo.NewHTTPError(http.StatusForbidden)
		}

		var hasMatchingFile bool
		stat, err := os.Stat(requestedFile)
		switch {
		case os.IsNotExist(err):
		case os.IsPermission(err):
			hasMatchingFile = false
		case err != nil:
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to check if file exists")
		default:
			hasMatchingFile = !stat.IsDir()
		}
		if hasMatchingFile {
			return c.File(requestedFile)
		}

		return c.File(reactIndex)
	})

	m.echo.Static("/api/v1/api.swagger.json",
		filepath.Join(m.config.Root, "swagger/determined/api/v1/api.swagger.json"))

	m.echo.GET("/config", api.Route(m.getConfig))
	m.echo.GET("/info", api.Route(m.getInfo))
	m.echo.GET("/logs", api.Route(m.getMasterLogs), authFuncs...)

	m.echo.GET("/experiment-list", api.Route(m.getExperimentList), authFuncs...)
	m.echo.GET("/experiment-summaries", api.Route(m.getExperimentSummaries), authFuncs...)

	experimentsGroup := m.echo.Group("/experiments", authFuncs...)
	experimentsGroup.GET("", api.Route(m.getExperiments))
	experimentsGroup.GET("/:experiment_id", api.Route(m.getExperiment))
	experimentsGroup.GET("/:experiment_id/checkpoints", api.Route(m.getExperimentCheckpoints))
	experimentsGroup.GET("/:experiment_id/config", api.Route(m.getExperimentConfig))
	experimentsGroup.GET("/:experiment_id/model_def", m.getExperimentModelDefinition)
	experimentsGroup.GET("/:experiment_id/preview_gc", api.Route(m.getExperimentCheckpointsToGC))
	experimentsGroup.GET("/:experiment_id/summary", api.Route(m.getExperimentSummary))
	experimentsGroup.GET("/:experiment_id/metrics/summary", api.Route(m.getExperimentSummaryMetrics))
	experimentsGroup.PATCH("/:experiment_id", api.Route(m.patchExperiment))
	experimentsGroup.POST("", api.Route(m.postExperiment))
	experimentsGroup.POST("/:experiment_id/kill", api.Route(m.postExperimentKill))
	experimentsGroup.DELETE("/:experiment_id", api.Route(m.deleteExperiment))

	searcherGroup := m.echo.Group("/searcher", authFuncs...)
	searcherGroup.POST("/preview", api.Route(m.getSearcherPreview))

	trialsGroup := m.echo.Group("/trials", authFuncs...)
	trialsGroup.GET("/:trial_id", api.Route(m.getTrial))
	trialsGroup.GET("/:trial_id/details", api.Route(m.getTrialDetails))
	trialsGroup.GET("/:trial_id/logs", m.getTrialLogs)
	trialsGroup.GET("/:trial_id/metrics", api.Route(m.getTrialMetrics))
	trialsGroup.GET("/:trial_id/logsv2", api.Route(m.getTrialLogsV2))
	trialsGroup.POST("/:trial_id/kill", api.Route(m.postTrialKill))

	checkpointsGroup := m.echo.Group("/checkpoints", authFuncs...)
	checkpointsGroup.GET("", api.Route(m.getCheckpoints))
	checkpointsGroup.GET("/:checkpoint_uuid", api.Route(m.getCheckpoint))
	checkpointsGroup.POST("/:checkpoint_uuid/metadata", api.Route(m.addCheckpointMetadata))
	checkpointsGroup.DELETE("/:checkpoint_uuid/metadata", api.Route(m.deleteCheckpointMetadata))

	m.echo.POST("/trial_logs", api.Route(m.postTrialLogs))

	m.echo.GET("/ws/trial/:experiment_id/:trial_id/:container_id",
		api.WebSocketRoute(m.trialWebSocket))

	m.echo.GET("/ws/data-layer/*",
		api.WebSocketRoute(m.rwCoordinatorWebSocket))

	m.echo.Any("/debug/pprof/*", echo.WrapHandler(http.HandlerFunc(pprof.Index)))
	m.echo.Any("/debug/pprof/cmdline", echo.WrapHandler(http.HandlerFunc(pprof.Cmdline)))
	m.echo.Any("/debug/pprof/profile", echo.WrapHandler(http.HandlerFunc(pprof.Profile)))
	m.echo.Any("/debug/pprof/symbol", echo.WrapHandler(http.HandlerFunc(pprof.Symbol)))
	m.echo.Any("/debug/pprof/trace", echo.WrapHandler(http.HandlerFunc(pprof.Trace)))

	handler := m.system.AskAt(actor.Addr("proxy"), proxy.NewProxyHandler{ServiceID: "service"})
	m.echo.Any("/proxy/:service/*", handler.Get().(echo.HandlerFunc))

	handler = m.system.AskAt(actor.Addr("proxy"), proxy.NewConnectHandler{})
	m.echo.CONNECT("*", handler.Get().(echo.HandlerFunc))

	user.RegisterAPIHandler(m.echo, userService, authFuncs...)
	command.RegisterAPIHandler(
		m.system,
		m.echo,
		m.db,
		m.proxy,
		m.config.TensorBoardTimeout,
		m.config.Security.DefaultTask,
		m.taskSpec,
		authFuncs...,
	)
	template.RegisterAPIHandler(m.echo, m.db, authFuncs...)

	if m.config.Telemetry.Enabled && m.config.Telemetry.SegmentMasterKey != "" {
		if telemetry, tErr := telemetry.NewActor(
			m.db,
			m.ClusterID,
			m.MasterID,
			m.Version,
			resourcemanagers.GetResourceManagerType(m.config.ResourceManager),
			m.config.Telemetry.SegmentMasterKey,
		); tErr != nil {
			// We wouldn't want to totally fail just because telemetry failed; just note the error.
			log.WithError(err).Errorf("failed to initialize telemetry")
		} else {
			log.Info("telemetry reporting is enabled; run with `--telemetry-enabled=false` to disable")
			m.system.ActorOf(actor.Addr("telemetry"), telemetry)
		}
	} else {
		log.Info("telemetry reporting is disabled")
	}

	masterURL, err := getMasterURL(m.config)
	if err != nil {
		return errors.Wrap(err, "couldn't parse masterURL")
	}

	var oauthService *oauth.Service
	if m.config.Scim.Enabled {
		log.Infof("OAuth is enabled at %s%s", masterURL, oauth.Root)
		oauthService, err = oauth.New(userService, m.db)
		if err != nil {
			return err
		}
		oauth.RegisterAPIHandler(m.echo, oauthService)
	} else {
		log.Info("OAuth is disabled")
	}

	if m.config.Scim.Enabled && m.config.Scim.Username != "" && m.config.Scim.Password != "" {
		log.Infof("SCIM is enabled at %v/scim/v2", masterURL)
		scim.RegisterAPIHandler(m.echo, m.db, &m.config.Scim, masterURL)
	} else {
		log.Info("SCIM is disabled")
	}

	if m.config.SAML.Enabled {
		log.Info("SAML is enabled")
		samlService, err := saml.New(m.db, m.config.SAML)
		if err != nil {
			return errors.Wrap(err, "error creating SAML service")
		}
		saml.RegisterAPIHandler(m.echo, samlService)
	} else {
		log.Info("SAML is disabled")
	}

	return m.startServers(cert)
}
