// @APIVersion 1.0.0
// @APITitle 9volt API
// @APIDescription 9volt's API for fetching check state, event data and cluster information
// @Contact daniel.selans@gmail.com
// @License MIT
// @LicenseUrl https://opensource.org/licenses/MIT
// @BasePath /api/v1
// @SubApi Cluster State [/cluster]
// @SubApi Monitor Configuration [/monitor]

package api

import (
	"net/http"
	"os"

	"github.com/InVisionApp/rye"
	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"

	"github.com/9corp/9volt/config"
)

type Api struct {
	Config       *config.Config
	MemberID     string
	Identifier   string
	MWHandler    *rye.MWHandler
	DebugUI      bool
	AccessTokens []string
}

type JSONStatus struct {
	Status  string
	Message string
}

func New(cfg *config.Config, mwHandler *rye.MWHandler, debugUI bool, accessTokens []string) *Api {
	return &Api{
		Config:       cfg,
		MemberID:     cfg.MemberID,
		Identifier:   "api",
		MWHandler:    mwHandler,
		DebugUI:      debugUI,
		AccessTokens: accessTokens,
	}
}

func (a *Api) Run() {
	log.Debugf("%v: Starting API server", a.Identifier)

	routes := mux.NewRouter().StrictSlash(true)

	// Necessary so we do not put /version and
	// /status/check behind access tokens
	basicHandler := rye.NewMWHandler(rye.Config{})

	routes.Handle(setupHandler(basicHandler,
		"/", []rye.Handler{
			a.HomeHandler,
		})).Methods("GET")

	// Common handlers
	routes.Handle(setupHandler(basicHandler,
		"/version", []rye.Handler{
			a.VersionHandler,
		})).Methods("GET")

	routes.Handle(setupHandler(basicHandler,
		"/status/check", []rye.Handler{
			a.StatusHandler,
		})).Methods("GET")

	// Prepend the access token middleware to every /api endpoint if
	// any access tokens were given
	//
	// TODO: UI will currently not work if access tokens are enabled
	if len(a.AccessTokens) != 0 {
		a.MWHandler.Use(rye.NewMiddlewareAccessToken("X-Access-Token", a.AccessTokens))
	}

	// State handlers (route order matters!)
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/state", []rye.Handler{
			a.StateWithTagsHandler,
		})).Methods("GET").Queries("tags", "")

	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/state", []rye.Handler{
			a.StateHandler,
		})).Methods("GET")

	// Cluster handlers
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/cluster", []rye.Handler{
			a.ClusterHandler,
		})).Methods("GET")

	// Monitor handlers (route order matters!)
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/monitor", []rye.Handler{
			a.MonitorHandler,
		})).Methods("GET")

	// Add monitor config
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/monitor", []rye.Handler{
			a.MonitorAddHandler,
		})).Methods("POST")

	// Disable a specific monitor config
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/monitor/{check}", []rye.Handler{
			a.MonitorDisableHandler,
		})).Methods("GET").Queries("disable", "")

	// Fetch a specific monitor config
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/monitor/{check}", []rye.Handler{
			a.MonitorCheckHandler,
		})).Methods("GET")

	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/monitor/{check}", []rye.Handler{
			a.MonitorDeleteHandler,
		})).Methods("DELETE")

	// Alerter handlers (route order matters!)
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/alerter", []rye.Handler{
			a.AlerterHandler,
		})).Methods("GET")

	// Add alerter config
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/alerter", []rye.Handler{
			a.AlerterAddHandler,
		})).Methods("POST")

	// Fetch a specific alerter config
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/alerter/{alerterName}", []rye.Handler{
			a.AlerterGetHandler,
		})).Methods("GET")

	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/alerter/{alerterName}", []rye.Handler{
			a.AlerterDeleteHandler,
		})).Methods("DELETE")

	// Events handlers
	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/event", []rye.Handler{
			a.EventWithTypeHandler,
		})).Methods("GET").Queries("type", "")

	routes.Handle(setupHandler(a.MWHandler,
		"/api/v1/event", []rye.Handler{
			a.EventHandler,
		})).Methods("GET")

	if a.DebugUI {
		log.Info("ui: local debug mode (from /ui/dist)")
		routes.PathPrefix("/dist").Handler(a.MWHandler.Handle([]rye.Handler{
			rye.MiddlewareRouteLogger(),
			a.uiDistHandler,
		}))

		routes.PathPrefix("/ui").Handler(a.MWHandler.Handle([]rye.Handler{
			rye.MiddlewareRouteLogger(),
			a.uiHandler,
		}))
	} else {
		log.Info("ui: statik mode (from statik.go)")
		routes.PathPrefix("/dist").Handler(a.MWHandler.Handle([]rye.Handler{
			rye.MiddlewareRouteLogger(),
			a.uiDistStatikHandler,
		}))

		routes.PathPrefix("/ui").Handler(a.MWHandler.Handle([]rye.Handler{
			rye.MiddlewareRouteLogger(),
			a.uiStatikHandler,
		}))
	}

	http.ListenAndServe(a.Config.ListenAddress, routes)
}

// appends an apache style logger to each route. also dry up some boiler plate
func setupHandler(mw *rye.MWHandler, path string, ryeStack []rye.Handler) (string, http.Handler) {
	return path, handlers.LoggingHandler(os.Stdout, mw.Handle(ryeStack))
}
