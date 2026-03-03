package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
)

var (
	xdsPort   uint
	httpPort  uint
	timeoutMs uint
	nodeID    string
)

func init() {
	flag.UintVar(&xdsPort, "xds-port", 5678, "gRPC xDS port")
	flag.UintVar(&httpPort, "http-port", 5679, "HTTP control port")
	flag.UintVar(&timeoutMs, "timeout-ms", 0, "host_removal_stabilization_timeout_ms (0=disabled)")
	flag.StringVar(&nodeID, "node-id", "test-node", "Envoy node ID")
}

func makeCluster(tms uint) *cluster.Cluster {
	c := &cluster.Cluster{
		Name:                 "test-cluster",
		ConnectTimeout:       durationpb.New(1 * time.Second),
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS},
		EdsClusterConfig: &cluster.Cluster_EdsClusterConfig{
			EdsConfig: &core.ConfigSource{
				ConfigSourceSpecifier: &core.ConfigSource_ApiConfigSource{
					ApiConfigSource: &core.ApiConfigSource{
						ApiType:             core.ApiConfigSource_GRPC,
						TransportApiVersion: resource.DefaultAPIVersion,
						GrpcServices: []*core.GrpcService{{
							TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
								EnvoyGrpc: &core.GrpcService_EnvoyGrpc{ClusterName: "xds_cluster"},
							},
						}},
					},
				},
				ResourceApiVersion: resource.DefaultAPIVersion,
			},
		},
		HealthChecks: []*core.HealthCheck{{
			Timeout:            durationpb.New(1 * time.Second),
			Interval:           durationpb.New(1 * time.Second),
			UnhealthyThreshold: &wrapperspb.UInt32Value{Value: 1},
			HealthyThreshold:   &wrapperspb.UInt32Value{Value: 1},
			HealthChecker: &core.HealthCheck_HttpHealthCheck_{
				HttpHealthCheck: &core.HealthCheck_HttpHealthCheck{
					Path: "/healthz",
				},
			},
		}},
	}

	if tms > 0 {
		s, _ := structpb.NewStruct(map[string]interface{}{
			"host_removal_stabilization_timeout_ms": float64(tms),
		})
		c.Metadata = &core.Metadata{
			FilterMetadata: map[string]*structpb.Struct{
				"envoy.eds": s,
			},
		}
	}

	return c
}

func makeEndpoints(ports []uint32) *endpoint.ClusterLoadAssignment {
	var lbEndpoints []*endpoint.LbEndpoint
	for _, port := range ports {
		lbEndpoints = append(lbEndpoints, &endpoint.LbEndpoint{
			HostIdentifier: &endpoint.LbEndpoint_Endpoint{
				Endpoint: &endpoint.Endpoint{
					Address: &core.Address{
						Address: &core.Address_SocketAddress{
							SocketAddress: &core.SocketAddress{
								Protocol: core.SocketAddress_TCP,
								Address:  "127.0.0.1",
								PortSpecifier: &core.SocketAddress_PortValue{
									PortValue: port,
								},
							},
						},
					},
				},
			},
		})
	}
	return &endpoint.ClusterLoadAssignment{
		ClusterName: "test-cluster",
		Endpoints: []*endpoint.LocalityLbEndpoints{{
			LbEndpoints: lbEndpoints,
		}},
	}
}

func main() {
	flag.Parse()

	snapshotCache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	ctx := context.Background()
	srv := serverv3.NewServer(ctx, snapshotCache, nil)

	version := &atomic.Int64{}

	// gRPC xDS server
	grpcServer := grpc.NewServer(
		grpc.MaxConcurrentStreams(1000),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 5 * time.Second,
		}),
	)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, srv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, srv)

	grpcLis, err := net.Listen("tcp", fmt.Sprintf(":%d", xdsPort))
	if err != nil {
		log.Fatalf("gRPC listen: %v", err)
	}
	go func() {
		log.Printf("xDS server listening on :%d", xdsPort)
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatalf("gRPC serve: %v", err)
		}
	}()

	// HTTP control server
	pushSnapshot := func(ports []uint32) error {
		v := fmt.Sprintf("%d", version.Add(1))
		c := makeCluster(timeoutMs)
		e := makeEndpoints(ports)
		snap, err := cachev3.NewSnapshot(v, map[resource.Type][]types.Resource{
			resource.ClusterType:  {c},
			resource.EndpointType: {e},
		})
		if err != nil {
			return fmt.Errorf("new snapshot: %w", err)
		}
		return snapshotCache.SetSnapshot(ctx, nodeID, snap)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/add-targets", func(w http.ResponseWriter, r *http.Request) {
		if err := pushSnapshot([]uint32{8081, 8082}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		log.Println("pushed snapshot: 2 targets (8081, 8082)")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/remove-targets", func(w http.ResponseWriter, r *http.Request) {
		if err := pushSnapshot(nil); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		log.Println("pushed snapshot: 0 targets")
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("HTTP control listening on :%d", httpPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", httpPort), mux))
}
