package entrypoints

import (
	"context"
	"crypto/tls"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpcauth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	"github.com/lyft/flyteadmin/pkg/auth"
	"github.com/lyft/flyteadmin/pkg/auth/interfaces"

	"github.com/lyft/flyteadmin/pkg/server"
	"github.com/pkg/errors"
	"google.golang.org/grpc/credentials"
	"net"
	"net/http"
	_ "net/http/pprof" // Required to serve application.
	"strings"

	"github.com/lyft/flyteadmin/pkg/common"

	"github.com/lyft/flytestdlib/logger"

	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	flyteService "github.com/lyft/flyteidl/gen/pb-go/flyteidl/service"

	"github.com/lyft/flyteadmin/pkg/config"
	"github.com/lyft/flyteadmin/pkg/rpc/adminservice"

	"github.com/spf13/cobra"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/lyft/flytestdlib/contextutils"
	"github.com/lyft/flytestdlib/promutils/labeled"
	"google.golang.org/grpc"
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Launches the Flyte admin server",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		serverConfig := config.GetConfig()

		if serverConfig.Security.Secure {
			return serveGatewaySecure(ctx, serverConfig)
		}
		return serveGatewayInsecure(ctx, serverConfig)
	},
}

func init() {
	// Command information
	RootCmd.AddCommand(serveCmd)

	// Set Keys
	labeled.SetMetricKeys(contextutils.AppNameKey, contextutils.ProjectKey, contextutils.DomainKey,
		contextutils.ExecIDKey, contextutils.WorkflowIDKey, contextutils.NodeIDKey, contextutils.TaskIDKey,
		contextutils.TaskTypeKey, common.RuntimeTypeKey, common.RuntimeVersionKey)
}

// Creates a new gRPC Server with all the configuration
func newGRPCServer(ctx context.Context, cfg *config.ServerConfig, authContext interfaces.AuthenticationContext,
	opts ...grpc.ServerOption) (*grpc.Server, error) {
	// Not yet implemented for streaming
	var chainedUnaryInterceptors grpc.UnaryServerInterceptor
	if cfg.Security.UseAuth {
		logger.Infof(ctx, "Creating gRPC server with authentication")
		chainedUnaryInterceptors = grpc_middleware.ChainUnaryServer(grpc_prometheus.UnaryServerInterceptor,
			auth.GetAuthenticationCustomMetadataInterceptor(authContext),
			grpcauth.UnaryServerInterceptor(auth.GetAuthenticationInterceptor(authContext)),
			auth.AuthenticationLoggingInterceptor,
		)
	} else {
		logger.Infof(ctx, "Creating gRPC server without authentication")
		chainedUnaryInterceptors = grpc_middleware.ChainUnaryServer(grpc_prometheus.UnaryServerInterceptor)
	}
	serverOpts := []grpc.ServerOption{
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		grpc.UnaryInterceptor(chainedUnaryInterceptors),
	}
	serverOpts = append(serverOpts, opts...)
	grpcServer := grpc.NewServer(serverOpts...)
	grpc_prometheus.Register(grpcServer)
	flyteService.RegisterAdminServiceServer(grpcServer, adminservice.NewAdminServer(cfg.KubeConfig, cfg.Master))
	return grpcServer, nil
}

func GetHandleOpenapiSpec(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		swaggerBytes, err := flyteService.Asset("admin.swagger.json")
		if err != nil {
			logger.Warningf(ctx, "Err %v", err)
			w.WriteHeader(http.StatusFailedDependency)
		} else {
			w.WriteHeader(http.StatusOK)
			_, err := w.Write(swaggerBytes)
			if err != nil {
				logger.Errorf(ctx, "failed to write openAPI information, error: %s", err.Error())
			}
		}
	}
}

func healthCheckFunc(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func newHTTPServer(ctx context.Context, cfg *config.ServerConfig, authContext interfaces.AuthenticationContext,
	grpcAddress string, grpcConnectionOpts ...grpc.DialOption) (*http.ServeMux, error) {

	// Register the server that will serve HTTP/REST Traffic
	mux := http.NewServeMux()

	// Register healthcheck
	mux.HandleFunc("/healthcheck", healthCheckFunc)

	// Register OpenAPI endpoint
	// This endpoint will serve the OpenAPI2 spec generated by the swagger protoc plugin, and bundled by go-bindata
	mux.HandleFunc("/api/v1/openapi", GetHandleOpenapiSpec(ctx))

	// Register the actual Server that will service gRPC traffic
	var gwmux *runtime.ServeMux
	if cfg.Security.UseAuth {
		// Add HTTP handlers for OAuth2 endpoints
		mux.HandleFunc("/login", auth.RefreshTokensIfExists(ctx, authContext,
			auth.GetLoginHandler(ctx, authContext)))
		mux.HandleFunc("/callback", auth.GetCallbackHandler(ctx, authContext))
		// Install the user info endpoint if there is a user info url configured.
		if authContext.GetUserInfoUrl() != nil && authContext.GetUserInfoUrl().String() != "" {
			mux.HandleFunc("/me", auth.GetMeEndpointHandler(ctx, authContext))
		}
		mux.HandleFunc(auth.MetadataEndpoint, auth.GetMetadataEndpointRedirectHandler(ctx, authContext))

		gwmux = runtime.NewServeMux(
			runtime.WithMarshalerOption("application/octet-stream", &runtime.ProtoMarshaller{}),
			runtime.WithMetadata(auth.GetHttpRequestCookieToMetadataHandler(authContext)))
	} else {
		gwmux = runtime.NewServeMux(
			runtime.WithMarshalerOption("application/octet-stream", &runtime.ProtoMarshaller{}))
	}

	// Enable CORS if necessary on the gateway mux
	noopPattern := runtime.MustPattern(runtime.NewPattern(1, []int{3, 0}, []string{}, ""))
	decorator := auth.GetCorsDecorator(ctx)
	gwmux.Handle(http.MethodOptions, noopPattern, func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		logger.Debugf(ctx, "Serving OPTIONS")
		decorator(http.HandlerFunc(healthCheckFunc)).ServeHTTP(w, r)
	})

	err := flyteService.RegisterAdminServiceHandlerFromEndpoint(ctx, gwmux, grpcAddress, grpcConnectionOpts)
	if err != nil {
		return nil, errors.Wrap(err, "error registering admin service")
	}

	mux.Handle("/", gwmux)

	return mux, nil
}

func serveGatewayInsecure(ctx context.Context, cfg *config.ServerConfig) error {
	logger.Infof(ctx, "Serving Flyte Admin Insecure")

	// This will parse configuration and create the necessary objects for dealing with auth
	var authContext interfaces.AuthenticationContext
	var err error
	// This code is here to support authentication without SSL. This setup supports a network topology where
	// Envoy does the SSL termination. The final hop is made over localhost only on a trusted machine.
	// Warning: Running authentication without SSL in any other topology is a severe security flaw.
	// See the auth.Config object for additional settings as well.
	if cfg.Security.UseAuth {
		authContext, err = auth.NewAuthenticationContext(ctx, cfg.Security.Oauth)
		if err != nil {
			logger.Errorf(ctx, "Error creating auth context %s", err)
			return err
		}
	}

	grpcServer, err := newGRPCServer(ctx, cfg, authContext)
	if err != nil {
		return errors.Wrap(err, "failed to create GRPC server")
	}

	logger.Infof(ctx, "Serving GRPC Traffic on: %s", cfg.GetGrpcHostAddress())
	lis, err := net.Listen("tcp", cfg.GetGrpcHostAddress())
	if err != nil {
		return errors.Wrapf(err, "failed to listen on GRPC port: %s", cfg.GetGrpcHostAddress())
	}

	go func() {
		err := grpcServer.Serve(lis)
		logger.Fatalf(ctx, "Failed to create GRPC Server, Err: ", err)
	}()

	logger.Infof(ctx, "Starting HTTP/1 Gateway server on %s", cfg.GetHostAddress())
	httpServer, err := newHTTPServer(ctx, cfg, authContext, cfg.GetGrpcHostAddress(), grpc.WithInsecure())
	if err != nil {
		return err
	}
	err = http.ListenAndServe(cfg.GetHostAddress(), httpServer)
	if err != nil {
		return errors.Wrapf(err, "failed to Start HTTP Server")
	}

	return nil
}

// grpcHandlerFunc returns an http.Handler that delegates to grpcServer on incoming gRPC
// connections or otherHandler otherwise.
// See https://github.com/philips/grpc-gateway-example/blob/master/cmd/serve.go for reference
func grpcHandlerFunc(grpcServer *grpc.Server, otherHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This is a partial recreation of gRPC's internal checks
		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			otherHandler.ServeHTTP(w, r)
		}
	})
}

func serveGatewaySecure(ctx context.Context, cfg *config.ServerConfig) error {
	certPool, cert, err := server.GetSslCredentials(ctx, cfg.Security.Ssl.CertificateFile, cfg.Security.Ssl.KeyFile)
	if err != nil {
		return err
	}
	// This will parse configuration and create the necessary objects for dealing with auth
	var authContext interfaces.AuthenticationContext
	if cfg.Security.UseAuth {
		authContext, err = auth.NewAuthenticationContext(ctx, cfg.Security.Oauth)
		if err != nil {
			logger.Errorf(ctx, "Error creating auth context %s", err)
			return err
		}
	}

	grpcServer, err := newGRPCServer(ctx, cfg, authContext,
		grpc.Creds(credentials.NewServerTLSFromCert(cert)))
	if err != nil {
		return errors.Wrap(err, "failed to create GRPC server")
	}

	// Whatever certificate is used, pass it along for easier development
	dialCreds := credentials.NewTLS(&tls.Config{
		ServerName: cfg.GetHostAddress(),
		RootCAs:    certPool,
	})
	httpServer, err := newHTTPServer(ctx, cfg, authContext, cfg.GetHostAddress(), grpc.WithTransportCredentials(dialCreds))
	if err != nil {
		return err
	}

	conn, err := net.Listen("tcp", cfg.GetHostAddress())
	if err != nil {
		panic(err)
	}

	srv := &http.Server{
		Addr:    cfg.GetHostAddress(),
		Handler: grpcHandlerFunc(grpcServer, httpServer),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{*cert},
			NextProtos:   []string{"h2"},
		},
	}

	err = srv.Serve(tls.NewListener(conn, srv.TLSConfig))

	if err != nil {
		return errors.Wrapf(err, "failed to Start HTTP/2 Server")
	}
	return nil
}
