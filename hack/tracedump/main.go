// tracedump is a minimal OTLP/gRPC trace collector for verifying propagation.
//
// It exists because "is the trace connected" is a question about relationships
// between spans from different processes, which cannot be answered from any one
// process's logs. Running a full collector to answer it means installing and
// configuring one; this prints the tree directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

type span struct {
	name     string
	service  string
	spanID   string
	parentID string
	startNs  uint64
	endNs    uint64
	events   []string
}

type collector struct {
	coltrace.UnimplementedTraceServiceServer

	mu     sync.Mutex
	traces map[string][]span
}

func (c *collector) Export(_ context.Context, req *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rs := range req.ResourceSpans {
		service := "unknown"
		for _, attr := range rs.Resource.GetAttributes() {
			if attr.Key == "service.name" {
				service = attr.Value.GetStringValue()
			}
		}
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				var events []string
				for _, e := range s.Events {
					events = append(events, e.Name)
				}
				tid := fmt.Sprintf("%x", s.TraceId)
				c.traces[tid] = append(c.traces[tid], span{
					name: s.Name, service: service,
					spanID:   fmt.Sprintf("%x", s.SpanId),
					parentID: fmt.Sprintf("%x", s.ParentSpanId),
					startNs:  s.StartTimeUnixNano, endNs: s.EndTimeUnixNano,
					events: events,
				})
			}
		}
		c.report()
	}
	return &coltrace.ExportTraceServiceResponse{}, nil
}

// report prints each trace as a tree. Printing on every export means a trace
// spanning several exports is reprinted as it fills in, which is what shows
// whether a later process's spans joined the same trace or started a new one.
func (c *collector) report() {
	for tid, spans := range c.traces {
		fmt.Printf("\n=== trace %s (%d spans) ===\n", tid, len(spans))
		byParent := map[string][]span{}
		for _, s := range spans {
			byParent[s.parentID] = append(byParent[s.parentID], s)
		}
		// A root here is a span whose parent is absent from this trace, not
		// only one with an empty parent: a process that failed to propagate
		// shows up as a second root, which is exactly the failure to surface.
		present := map[string]bool{}
		for _, s := range spans {
			present[s.spanID] = true
		}
		var roots []span
		for _, s := range spans {
			if s.parentID == "" || s.parentID == "0000000000000000" || !present[s.parentID] {
				roots = append(roots, s)
			}
		}
		sort.Slice(roots, func(i, j int) bool { return roots[i].startNs < roots[j].startNs })
		for _, r := range roots {
			printTree(r, byParent, 0)
		}
		if len(roots) > 1 {
			fmt.Printf("  !! %d roots: some hop did not propagate context\n", len(roots))
		}
	}
}

func printTree(s span, byParent map[string][]span, depth int) {
	indent := ""
	for range depth {
		indent += "  "
	}
	ms := float64(s.endNs-s.startNs) / 1e6
	fmt.Printf("%s%-28s %-10s %8.1fms", indent, s.name, s.service, ms)
	if len(s.events) > 0 {
		fmt.Printf("  events=%v", s.events)
	}
	fmt.Println()
	kids := byParent[s.spanID]
	sort.Slice(kids, func(i, j int) bool { return kids[i].startNs < kids[j].startNs })
	for _, k := range kids {
		printTree(k, byParent, depth+1)
	}
}

func main() {
	addr := flag.String("listen", "127.0.0.1:4317", "OTLP/gRPC listen address")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	srv := grpc.NewServer()
	coltrace.RegisterTraceServiceServer(srv, &collector{traces: map[string][]span{}})
	log.Printf("tracedump listening on %s", *addr)
	log.Fatal(srv.Serve(lis))
}
