// gRPC service wrapping NoScriptRuleStore.Lookup. This is the whole surface a
// pure client sees: one RPC, three scalar request fields, no Redis exposure.
package router

import (
	"context"

	pb "routerservice/proto"
)

type Server struct {
	pb.UnimplementedRouterServer
	Store *NoScriptRuleStore
}

func NewServer(store *NoScriptRuleStore) *Server {
	return &Server{Store: store}
}

func (s *Server) Lookup(ctx context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	mode := SingleLevel
	if req.Mode == pb.MatchMode_MULTI_LEVEL {
		mode = MultiLevel
	}
	match, err := s.Store.Lookup(req.Fqdn1, int(req.Port), req.Fqdn2, mode)
	if err != nil {
		return nil, err
	}
	if match == nil {
		return &pb.LookupResponse{Hit: false}, nil
	}
	return &pb.LookupResponse{
		Hit:          true,
		RuleId:       match.RuleID,
		Fqdn1Pattern: match.Fqdn1Pattern,
		Fqdn2Pattern: match.Fqdn2Pattern,
		Rank1:        int32(match.Rank1),
		Rank2:        int32(match.Rank2),
	}, nil
}
