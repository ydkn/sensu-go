package apid

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sensu/sensu-go/backend/apid/actions"
	"github.com/sensu/sensu-go/backend/apid/middlewares"
	"github.com/sensu/sensu-go/backend/apid/routers"
	"github.com/sensu/sensu-go/backend/authentication"
	"github.com/sensu/sensu-go/backend/authorization/rbac"
	"github.com/sensu/sensu-go/backend/messaging"
	"github.com/sensu/sensu-go/backend/store"
	"github.com/sensu/sensu-go/types"
)

// APId is the backend HTTP API.
type APId struct {
	Authenticator    *authentication.Authenticator
	HTTPServer       *http.Server
	CoreSubrouter    *mux.Router
	GraphQLSubrouter *mux.Router

	stopping            chan struct{}
	running             *atomic.Value
	wg                  *sync.WaitGroup
	errChan             chan error
	bus                 messaging.MessageBus
	store               store.Store
	eventStore          store.EventStore
	queueGetter         types.QueueGetter
	tls                 *types.TLSOptions
	cluster             clientv3.Cluster
	etcdClientTLSConfig *tls.Config
	clusterVersion      string
}

// Option is a functional option.
type Option func(*APId) error

// Config configures APId.
type Config struct {
	ListenAddress       string
	URL                 string
	Bus                 messaging.MessageBus
	Store               store.Store
	EventStore          store.EventStore
	QueueGetter         types.QueueGetter
	TLS                 *types.TLSOptions
	Cluster             clientv3.Cluster
	EtcdClientTLSConfig *tls.Config
	Authenticator       *authentication.Authenticator
	ClusterVersion      string
}

// New creates a new APId.
func New(c Config, opts ...Option) (*APId, error) {
	a := &APId{
		store:               c.Store,
		eventStore:          c.EventStore,
		queueGetter:         c.QueueGetter,
		tls:                 c.TLS,
		bus:                 c.Bus,
		stopping:            make(chan struct{}, 1),
		running:             &atomic.Value{},
		wg:                  &sync.WaitGroup{},
		errChan:             make(chan error, 1),
		cluster:             c.Cluster,
		etcdClientTLSConfig: c.EtcdClientTLSConfig,
		Authenticator:       c.Authenticator,
		clusterVersion:      c.ClusterVersion,
	}

	// prepare TLS config
	var tlsServerConfig *tls.Config
	var err error
	if c.TLS != nil {
		tlsServerConfig, err = c.TLS.ToServerTLSConfig()
		if err != nil {
			return nil, err
		}
	}

	router := NewRouter()
	_ = PublicSubrouter(router, c)
	a.GraphQLSubrouter = GraphQLSubrouter(router, c)
	_ = AuthenticationSubrouter(router, c)
	a.CoreSubrouter = CoreSubrouter(router, c)

	a.HTTPServer = &http.Server{
		Addr:         c.ListenAddress,
		Handler:      router,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
		TLSConfig:    tlsServerConfig,
	}

	for _, o := range opts {
		if err := o(a); err != nil {
			return nil, err
		}
	}

	return a, nil
}

// NewRouter creates a new mux router that implements the http.Handler interface
// and serves all requests
func NewRouter() *mux.Router {
	router := mux.NewRouter().UseEncodedPath()

	// Register a default handler when no routes match
	router.NotFoundHandler = middlewares.SimpleLogger{}.Then(http.HandlerFunc(notFoundHandler))

	return router
}

// AuthenticationSubrouter initializes a subrouter that handles all
// authentication requests
func AuthenticationSubrouter(router *mux.Router, cfg Config) *mux.Router {
	subrouter := NewSubrouter(
		router.NewRoute(),
		middlewares.SimpleLogger{},
		middlewares.RefreshToken{},
		middlewares.LimitRequest{},
	)

	mountRouters(subrouter,
		routers.NewAuthenticationRouter(cfg.Store, cfg.Authenticator),
	)

	return subrouter
}

// CoreSubrouter initializes a subrouter that handles all requests coming to
// /api/core/v2
func CoreSubrouter(router *mux.Router, cfg Config) *mux.Router {
	subrouter := NewSubrouter(
		router.PathPrefix("/api/{group:core}/{version:v2}/"),
		middlewares.SimpleLogger{},
		middlewares.Namespace{},
		middlewares.Authentication{},
		middlewares.AllowList{Store: cfg.Store},
		middlewares.AuthorizationAttributes{},
		middlewares.Authorization{Authorizer: &rbac.Authorizer{Store: cfg.Store}},
		middlewares.LimitRequest{},
		middlewares.Pagination{},
	)
	mountRouters(
		subrouter,
		routers.NewAssetRouter(cfg.Store),
		routers.NewChecksRouter(cfg.Store, cfg.QueueGetter),
		routers.NewClusterRolesRouter(cfg.Store),
		routers.NewClusterRoleBindingsRouter(cfg.Store),
		routers.NewClusterRouter(actions.NewClusterController(cfg.Cluster, cfg.Store)),
		routers.NewEntitiesRouter(cfg.Store, cfg.EventStore),
		routers.NewEventFiltersRouter(cfg.Store),
		routers.NewEventsRouter(cfg.EventStore, cfg.Bus),
		routers.NewExtensionsRouter(cfg.Store),
		routers.NewHandlersRouter(cfg.Store),
		routers.NewHooksRouter(cfg.Store),
		routers.NewMutatorsRouter(cfg.Store),
		routers.NewNamespacesRouter(cfg.Store),
		routers.NewRolesRouter(cfg.Store),
		routers.NewRoleBindingsRouter(cfg.Store),
		routers.NewSilencedRouter(cfg.Store),
		routers.NewTessenRouter(actions.NewTessenController(cfg.Store, cfg.Bus)),
		routers.NewUsersRouter(cfg.Store),
	)

	return subrouter
}

// GraphQLSubrouter initializes a subrouter that handles all requests for
// GraphQL
func GraphQLSubrouter(router *mux.Router, cfg Config) *mux.Router {
	subrouter := NewSubrouter(
		router.NewRoute(),
		middlewares.SimpleLogger{},
		middlewares.LimitRequest{},
		// TODO: Currently the web app relies on receiving a 401 to determine if
		//       a user is not authenticated. However, in the future we should
		//       allow requests without an access token to continue so that
		//       unauthenticated clients can still fetch the schema. Useful for
		//       implementing tools like GraphiQL.
		//
		//       https://github.com/graphql/graphiql
		//       https://graphql.org/learn/introspection/
		middlewares.Authentication{IgnoreUnauthorized: false},
		middlewares.AllowList{Store: cfg.Store, IgnoreMissingClaims: true},
	)

	mountRouters(
		subrouter,
		routers.NewGraphQLRouter(
			cfg.Store, cfg.EventStore, &rbac.Authorizer{Store: cfg.Store}, cfg.QueueGetter, cfg.Bus,
		),
	)

	return subrouter
}

// PublicSubrouter initializes a subrouter that handles all requests to public
// endpoints
func PublicSubrouter(router *mux.Router, cfg Config) *mux.Router {
	subrouter := NewSubrouter(
		router.NewRoute(),
		middlewares.SimpleLogger{},
		middlewares.LimitRequest{},
	)

	mountRouters(subrouter,
		routers.NewHealthRouter(
			actions.NewHealthController(cfg.Store, cfg.Cluster, cfg.EtcdClientTLSConfig),
		),
		routers.NewVersionRouter(actions.NewVersionController(cfg.ClusterVersion)),
		routers.NewTessenMetricRouter(actions.NewTessenMetricController(cfg.Bus)),
	)

	subrouter.Handle("/metrics", promhttp.Handler())

	return subrouter
}

func notFoundHandler(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	resp := map[string]interface{}{
		"message": "not found", "code": actions.NotFound,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// Start APId.
func (a *APId) Start() error {
	logger.Info("starting apid on address: ", a.HTTPServer.Addr)
	a.wg.Add(1)

	go func() {
		defer a.wg.Done()
		var err error
		if a.tls != nil {
			// TLS configuration comes from ToServerTLSConfig
			err = a.HTTPServer.ListenAndServeTLS("", "")
		} else {
			err = a.HTTPServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			a.errChan <- fmt.Errorf("failed to start http/https server %s", err)
		}
	}()

	return nil
}

// Stop httpApi.
func (a *APId) Stop() error {
	if err := a.HTTPServer.Shutdown(context.TODO()); err != nil {
		// failure/timeout shutting down the server gracefully
		logger.Error("failed to shutdown http server gracefully - forcing shutdown")
		if closeErr := a.HTTPServer.Close(); closeErr != nil {
			logger.Error("failed to shutdown http server forcefully")
		}
	}

	a.running.Store(false)
	close(a.stopping)
	a.wg.Wait()
	close(a.errChan)

	return nil
}

// Err returns a channel to listen for terminal errors on.
func (a *APId) Err() <-chan error {
	return a.errChan
}

// Name returns the daemon name
func (a *APId) Name() string {
	return "apid"
}

func mountRouters(parent *mux.Router, subRouters ...routers.Router) {
	for _, subRouter := range subRouters {
		subRouter.Mount(parent)
	}
}
