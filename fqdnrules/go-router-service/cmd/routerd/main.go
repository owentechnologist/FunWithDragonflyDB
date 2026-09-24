// routerd is the microservice binary: a stateless gRPC front end over
// NoScriptRuleStore. Run many of these behind a load balancer (or point a
// benchmark client's -target grpc=host:port[,host:port,...] at several of
// them directly) to test scaling the matching tier independently of
// Dragonfly's own thread/shard count. No local cache, no in-memory rule
// state -- every Lookup call hits Dragonfly. That statelessness is the
// architecture under test; adding a cache here would confound it.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	router "routerservice"
	pb "routerservice/proto"
)

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	var (
		listenAddr = flag.String("listen", ":9090", "gRPC listen address")
		uri        = flag.String("uri", "", "redis:// or rediss:// URI for the backing Dragonfly/Redis instance")
		host       = flag.String("host", "localhost", "redis/dragonfly host, used if -uri is empty")
		port       = flag.Int("port", 6379, "redis/dragonfly port, used if -uri is empty")
		db         = flag.Int("db", 0, "redis/dragonfly database index, used if -uri is empty")
		namespace  = flag.String("namespace", "bench", "key namespace, must match the writer's namespace")
		numBuckets = flag.Int("num-buckets", 16, "replicas for the global/tld catch-all tiers; must match what the writer used")
		poolSize   = flag.Int("pool-size", 0, "Dragonfly connection pool size; 0 picks 10*GOMAXPROCS")
	)
	flag.Parse()

	var opts *redis.Options
	if *uri != "" {
		parsed, err := redis.ParseURL(*uri)
		if err != nil {
			fail("parsing -uri: %v", err)
		}
		opts = parsed
	} else {
		opts = &redis.Options{Addr: fmt.Sprintf("%s:%d", *host, *port), DB: *db}
	}
	if *poolSize > 0 {
		opts.PoolSize = *poolSize
	} else {
		opts.PoolSize = 10 * runtime.GOMAXPROCS(0)
	}

	conn := redis.NewClient(opts)
	defer conn.Close()
	ctx := context.Background()
	if err := conn.Ping(ctx).Err(); err != nil {
		fail("cannot reach redis/dragonfly: %v", err)
	}

	store, err := router.NewNoScriptRuleStore(ctx, conn, *namespace, *numBuckets)
	if err != nil {
		fail("%v", err)
	}

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fail("listening on %s: %v", *listenAddr, err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterRouterServer(grpcServer, router.NewServer(store))

	fmt.Printf("routerd listening on %s, namespace=%q num-buckets=%d pool-size=%d\n",
		*listenAddr, *namespace, *numBuckets, opts.PoolSize)
	if err := grpcServer.Serve(lis); err != nil {
		fail("serving: %v", err)
	}
}
