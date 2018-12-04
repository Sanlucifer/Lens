package server

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	"github.com/RTradeLtd/Lens"
	"github.com/RTradeLtd/Lens/analyzer/images"
	"github.com/RTradeLtd/config"
	"github.com/RTradeLtd/rtfs"

	pb "github.com/RTradeLtd/grpc/lens"
	pbreq "github.com/RTradeLtd/grpc/lens/request"
	pbresp "github.com/RTradeLtd/grpc/lens/response"

	"github.com/RTradeLtd/grpc/middleware"
	"github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	context "golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// APIServer is the Lens API server
type APIServer struct {
	lens *lens.Service

	l *zap.SugaredLogger
}

// Run is used to create our API server
func Run(
	ctx context.Context,
	addr string,
	opts lens.ConfigOpts,
	cfg config.TemporalConfig,
	logger *zap.SugaredLogger,
) error {
	// instantiate ipfs connection
	ipfsAPI := fmt.Sprintf("%s:%s", cfg.IPFS.APIConnection.Host, cfg.IPFS.APIConnection.Port)
	logger.Infow("instantiating IPFS connection",
		"ipfs.api", ipfsAPI)
	manager, err := rtfs.NewManager(ipfsAPI, nil, 1*time.Minute)
	if err != nil {
		return fmt.Errorf("failed to instantiate ipfs manager: %s", err.Error())
	}

	// instantiate tensorflow wrappers
	logger.Infow("instantiating tensorflow wrappers",
		"tensorflow.models", opts.ModelsPath)
	ia, err := images.NewAnalyzer(images.ConfigOpts{
		ModelLocation: opts.ModelsPath,
	}, logger.Named("analyzer").Named("images"))
	if err != nil {
		return fmt.Errorf("failed to instantiate image analyzer: %s", err.Error())
	}

	// create our lens service
	logger.Info("instantiating lens service")
	service, err := lens.NewService(opts, cfg, manager, ia, logger)
	if err != nil {
		return err
	}

	// create connection we will listen on
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// setup authentication interceptor
	unaryIntercept, streamInterceptor := middleware.NewServerInterceptors(cfg.Endpoints.Lens.AuthKey)
	// setup server options
	serverOpts := []grpc.ServerOption{
		grpc_middleware.WithUnaryServerChain(
			unaryIntercept,
			grpc_ctxtags.UnaryServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor))),
		grpc_middleware.WithStreamServerChain(
			streamInterceptor,
			grpc_ctxtags.StreamServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor))),
	}

	// setup tls configuration
	if cfg.Lens.TLS.CertPath != "" {
		logger.Infow("setting up TLS",
			"cert", cfg.TLS.CertPath,
			"key", cfg.TLS.KeyPath)
		creds, err := credentials.NewServerTLSFromFile(
			cfg.Endpoints.Lens.TLS.CertPath,
			cfg.Endpoints.Lens.TLS.KeyFile)
		if err != nil {
			return err
		}
		serverOpts = append(serverOpts, grpc.Creds(creds))
	} else {
		logger.Warn("no TLS configuration found")
	}

	// create a grpc server
	var s = &APIServer{
		lens: service,
		l:    logger,
	}
	gServer := grpc.NewServer(serverOpts...)
	pb.RegisterIndexerAPIServer(gServer, s)

	// interrupt server gracefully if context is cancelled
	go func() {
		for {
			select {
			case <-ctx.Done():
				logger.Info("shutting down server")
				gServer.GracefulStop()
				return
			}
		}
	}()

	// spin up server
	logger.Infow("spinning up server",
		"address", addr)
	if err = gServer.Serve(lis); err != nil {
		logger.Warn("shutting down server",
			"error", err)
		return err
	}
	return nil
}

// Index is used to submit a request for something to be indexed by lens
func (as *APIServer) Index(ctx context.Context, req *pbreq.Index) (*pbresp.Index, error) {
	switch req.GetType() {
	case "ipld":
		break
	default:
		return nil, errors.New("invalid data type")
	}

	var objectID = req.GetIdentifier()
	var reindex = req.GetReindex()
	metaData, err := as.lens.Magnify(objectID, reindex)
	if err != nil {
		return nil, err
	}

	var resp *lens.IndexOperationResponse
	if !reindex {
		if resp, err = as.lens.Store(metaData, objectID); err != nil {
			return nil, err
		}
	} else {
		b, err := as.lens.Get(objectID)
		if err != nil {
			return nil, fmt.Errorf("failed to find ID for object '%s'", objectID)
		}
		id, err := uuid.FromBytes(b)
		if err != nil {
			return nil, fmt.Errorf("invalid uuid found for '%s' ('%s'): %s",
				objectID, string(b), err.Error())
		}
		if resp, err = as.lens.Update(metaData, id, objectID); err != nil {
			return nil, err
		}
	}

	return &pbresp.Index{
		Id:       resp.LensID.String(),
		Keywords: metaData.Summary,
	}, nil
}

// Search is used to submit a simple search request against the lens index
func (as *APIServer) Search(ctx context.Context, req *pbreq.Search) (*pbresp.Results, error) {
	objects, err := as.lens.KeywordSearch(req.Keywords)
	if err != nil {
		return nil, err
	}

	var objs = make([]*pbresp.Object, len(objects))
	for _, v := range objects {
		objs = append(objs, &pbresp.Object{
			Name:     v.Name,
			MimeType: v.MetaData.MimeType,
			Category: v.MetaData.Category,
		})
	}

	return &pbresp.Results{
		Objects: objs,
	}, nil
}
