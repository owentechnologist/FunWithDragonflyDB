// Lookuper decouples the benchmark's query phase from which architecture is
// answering it, so the same query generation, latency measurement, and
// reporting code in main.go can drive either arch 2 (*RuleStore, direct to
// Dragonfly with Lua) or arch 3 (GRPCLookuper, against the no-Lua
// go-router-service microservice fleet) and produce directly comparable
// reports. *RuleStore already satisfies this via its existing Lookup method.
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "routerservice/proto"
)

type Lookuper interface {
	Lookup(fqdn1 string, port int, fqdn2 string, mode MatchMode) (*Match, error)
}

// PoolStatsLookuper is an optional capability: *RuleStore (arch 2, direct to
// Dragonfly) satisfies it since it owns a real go-redis connection pool.
// GRPCLookuper (arch 3) doesn't -- it has no local Redis connection at all,
// routerd owns that pool instead -- so callers must type-assert rather than
// require this on the main Lookuper interface.
type PoolStatsLookuper interface {
	PoolStats() *redis.PoolStats
}

// GRPCLookuper is the "pure client" side of architecture 3: it knows only the
// routerd addresses, never Dragonfly's own host, credentials, or key layout.
// Concurrent callers round-robin across every address, so pointing -target at
// several routerd instances simulates many stateless replicas ganging up on
// Dragonfly, the scaling model that architecture is built around.
type GRPCLookuper struct {
	ctx     context.Context
	clients []pb.RouterClient
	conns   []*grpc.ClientConn
	next    atomic.Uint64
}

func NewGRPCLookuper(ctx context.Context, addrs []string) (*GRPCLookuper, error) {
	l := &GRPCLookuper{ctx: ctx}
	for _, addr := range addrs {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("dialing routerd at %s: %w", addr, err)
		}
		l.conns = append(l.conns, conn)
		l.clients = append(l.clients, pb.NewRouterClient(conn))
	}
	return l, nil
}

func (l *GRPCLookuper) Close() {
	for _, conn := range l.conns {
		conn.Close()
	}
}

func (l *GRPCLookuper) Lookup(fqdn1 string, port int, fqdn2 string, mode MatchMode) (*Match, error) {
	pbMode := pb.MatchMode_SINGLE_LEVEL
	if mode == MultiLevel {
		pbMode = pb.MatchMode_MULTI_LEVEL
	}
	idx := int(l.next.Add(1)-1) % len(l.clients)
	resp, err := l.clients[idx].Lookup(l.ctx, &pb.LookupRequest{
		Fqdn1: fqdn1, Port: int32(port), Fqdn2: fqdn2, Mode: pbMode,
	})
	if err != nil {
		return nil, err
	}
	if !resp.Hit {
		return nil, nil
	}
	return &Match{
		RuleID:       resp.RuleId,
		Fqdn1Pattern: resp.Fqdn1Pattern,
		Port:         port,
		Fqdn2Pattern: resp.Fqdn2Pattern,
		Rank1:        int(resp.Rank1),
		Rank2:        int(resp.Rank2),
	}, nil
}
