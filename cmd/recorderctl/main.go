// Command recorderctl is the operator's view of the recorder: plain tables,
// a one-line verdict, and it opens its own port-forward when needed.
//
//	recorderctl status                 is anything wrong right now?
//	recorderctl diff [node]            what differs from the last healthy baseline
//	recorderctl timeline <node> [-since 6h]
//	recorderctl incidents [-all]
//	recorderctl events [-since 1h] [-node n] [-namespace ns]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

var (
	base      = flag.String("url", envOr("RECORDER_URL", "http://localhost:8080"), "collector URL; a port-forward is opened automatically when it is unreachable")
	namespace = flag.String("n", envOr("RECORDER_NAMESPACE", "recorder"), "namespace of the collector Service")
	service   = flag.String("svc", envOr("RECORDER_SERVICE", "recorder"), "collector Service name")
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: recorderctl [flags] status|diff [node]|timeline <node>|incidents|events")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	stop := connect()
	defer stop()
	var err error
	switch args[0] {
	case "status":
		err = status()
	case "diff":
		err = diff(args[1:])
	case "timeline":
		err = timeline(args[1:])
	case "incidents":
		err = incidents(args[1:])
	case "events":
		err = events(args[1:])
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// connect returns a cleanup func. If the URL does not answer, it starts
// `kubectl port-forward` on a free port and points base at it.
func connect() func() {
	if ping(*base) {
		return func() {}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return func() {}
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command("kubectl", "-n", *namespace, "port-forward", "svc/"+*service, fmt.Sprintf("%d:8080", port))
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach %s and cannot run kubectl: %v\n", *base, err)
		os.Exit(1)
	}
	*base = fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; i < 50 && !ping(*base); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !ping(*base) {
		_ = cmd.Process.Kill()
		fmt.Fprintf(os.Stderr, "port-forward to svc/%s in %s did not come up; is the collector running?\n", *service, *namespace)
		os.Exit(1)
	}
	return func() { _ = cmd.Process.Kill() }
}

func ping(url string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url + "/api/v1/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func get(path string, into any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", *base+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: %d %s", path, resp.StatusCode, e.Error)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

type field struct {
	State  string `json:"state"`
	Value  any    `json:"value"`
	Reason string `json:"reason"`
}

func (f *field) String() string {
	if f == nil {
		return "-"
	}
	switch f.State {
	case "present":
		return fmt.Sprint(f.Value)
	case "absent":
		return "ABSENT"
	case "unknown":
		return "unknown (" + f.Reason + ")"
	default:
		return "n/a"
	}
}

type nodeSummary struct {
	Node        string     `json:"node"`
	LastAPI     *time.Time `json:"last_api_snapshot"`
	LastAgent   *time.Time `json:"last_agent_snapshot"`
	Driver      string     `json:"driver"`
	HasBaseline bool       `json:"has_baseline"`
}

type incident struct {
	ID      int64      `json:"id"`
	Opened  time.Time  `json:"opened"`
	Closed  *time.Time `json:"closed"`
	Pods    []string   `json:"pods"`
	Changes []change   `json:"changes"`
}

type change struct {
	Time     time.Time `json:"time"`
	Node     string    `json:"node"`
	Field    string    `json:"field"`
	Old      field     `json:"old"`
	New      field     `json:"new"`
	Severity string    `json:"severity"`
}

type diffResp struct {
	Sources map[string]struct {
		BaselineTime *time.Time `json:"baseline_time"`
		Note         string     `json:"note"`
		Fields       []struct {
			Field    string `json:"field"`
			Baseline *field `json:"baseline"`
			Current  *field `json:"current"`
			Changed  bool   `json:"changed"`
		} `json:"fields"`
	} `json:"sources"`
}

func age(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return time.Since(*t).Round(time.Second).String() + " ago"
}

func table() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0) }

func status() error {
	var nodes []nodeSummary
	if err := get("/api/v1/nodes", &nodes); err != nil {
		return err
	}
	var open []incident
	if err := get("/api/v1/incidents?open=true", &open); err != nil {
		return err
	}
	w := table()
	fmt.Fprintln(w, "NODE\tAPI SNAPSHOT\tAGENT SNAPSHOT\tDRIVER\tBASELINE\tCHANGED VS BASELINE")
	var problems []string
	for _, n := range nodes {
		changed := "-"
		if n.HasBaseline {
			var d diffResp
			if err := get("/api/v1/nodes/"+n.Node+"/diff", &d); err == nil {
				var names []string
				for _, s := range d.Sources {
					for _, f := range s.Fields {
						if f.Changed {
							names = append(names, f.Field)
						}
					}
				}
				sort.Strings(names)
				if len(names) == 0 {
					changed = "none"
				} else {
					changed = strings.Join(names, ", ")
					problems = append(problems, fmt.Sprintf("%s differs from baseline: %s", n.Node, changed))
				}
			}
		}
		driver := n.Driver
		if driver == "" {
			driver = "-"
		}
		if n.LastAgent != nil && time.Since(*n.LastAgent) > 2*time.Minute {
			problems = append(problems, fmt.Sprintf("%s: agent silent for %s", n.Node, age(n.LastAgent)))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%s\n", n.Node, age(n.LastAPI), age(n.LastAgent), driver, n.HasBaseline, changed)
	}
	w.Flush()
	fmt.Println()
	for _, inc := range open {
		problems = append(problems, fmt.Sprintf("incident #%d open since %s: %d pods (%s)", inc.ID, inc.Opened.Local().Format("15:04"), len(inc.Pods), strings.Join(inc.Pods, ", ")))
	}
	if len(problems) == 0 {
		fmt.Printf("OK: %d nodes, no open incidents, nothing differs from baseline\n", len(nodes))
		return nil
	}
	fmt.Println("ATTENTION:")
	for _, p := range problems {
		fmt.Println("  -", p)
	}
	fmt.Println("\nnext: recorderctl diff <node>   then   recorderctl timeline <node> -since 6h")
	return nil
}

func diff(args []string) error {
	var nodes []string
	if len(args) > 0 {
		nodes = args
	} else {
		var ns []nodeSummary
		if err := get("/api/v1/nodes", &ns); err != nil {
			return err
		}
		for _, n := range ns {
			nodes = append(nodes, n.Node)
		}
	}
	for _, node := range nodes {
		var d diffResp
		if err := get("/api/v1/nodes/"+node+"/diff", &d); err != nil {
			return err
		}
		fmt.Printf("== %s\n", node)
		w := table()
		any := false
		for _, src := range []string{"api", "agent"} {
			s, ok := d.Sources[src]
			if !ok {
				continue
			}
			if s.Note != "" {
				fmt.Fprintf(w, "  %s\t%s\n", src, s.Note)
				continue
			}
			for _, f := range s.Fields {
				if f.Changed {
					any = true
					fmt.Fprintf(w, "  %s\t%s\t%s\t->\t%s\n", src, f.Field, f.Baseline.String(), f.Current.String())
				}
			}
		}
		w.Flush()
		if !any {
			fmt.Println("  matches baseline")
		}
	}
	return nil
}

func timeline(args []string) error {
	fs := flag.NewFlagSet("timeline", flag.ExitOnError)
	since := fs.String("since", "6h", "how far back (e.g. 30m, 6h, 2d is not valid: use 48h)")
	if len(args) == 0 {
		return fmt.Errorf("usage: recorderctl timeline <node> [-since 6h]")
	}
	_ = fs.Parse(args[1:])
	var entries []struct {
		Time time.Time       `json:"time"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	if err := get("/api/v1/nodes/"+args[0]+"/timeline?from=-"+*since, &entries); err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Printf("nothing recorded on %s in the last %s\n", args[0], *since)
		return nil
	}
	w := table()
	fmt.Fprintln(w, "TIME\tKIND\tWHAT")
	for _, e := range entries {
		var what string
		switch e.Kind {
		case "change":
			var c change
			_ = json.Unmarshal(e.Data, &c)
			what = fmt.Sprintf("%s  %s: %s -> %s", strings.ToUpper(c.Severity), c.Field, c.Old.String(), c.New.String())
		case "event":
			var ev struct {
				Type, Reason, ObjName, Message string
				Count                          int
			}
			_ = json.Unmarshal(e.Data, &ev)
			what = fmt.Sprintf("%s %s %s x%d: %s", ev.Type, ev.Reason, ev.ObjName, ev.Count, trunc(ev.Message, 70))
		case "pod_transition":
			var t struct{ Namespace, Name, Kind, Old, New, Detail string }
			_ = json.Unmarshal(e.Data, &t)
			what = fmt.Sprintf("%s/%s %s: %q -> %q %s", t.Namespace, t.Name, t.Kind, t.Old, t.New, t.Detail)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.Time.Local().Format("Jan 02 15:04:05"), e.Kind, what)
	}
	return w.Flush()
}

func incidents(args []string) error {
	fs := flag.NewFlagSet("incidents", flag.ExitOnError)
	all := fs.Bool("all", false, "include closed incidents (last 30 days)")
	_ = fs.Parse(args)
	path := "/api/v1/incidents?open=true"
	if *all {
		path = "/api/v1/incidents"
	}
	var incs []incident
	if err := get(path, &incs); err != nil {
		return err
	}
	if len(incs) == 0 {
		fmt.Println("no incidents")
		return nil
	}
	for _, inc := range incs {
		state := "OPEN"
		if inc.Closed != nil {
			state = "closed " + inc.Closed.Local().Format("Jan 02 15:04")
		}
		fmt.Printf("#%d  opened %s  %s\n  pods: %s\n", inc.ID, inc.Opened.Local().Format("Jan 02 15:04"), state, strings.Join(inc.Pods, ", "))
		if len(inc.Changes) > 0 {
			fmt.Println("  changes in the 6h before:")
			for _, c := range inc.Changes {
				fmt.Printf("    %s %s %s %s: %s -> %s\n", c.Time.Local().Format("15:04:05"), c.Node, strings.ToUpper(c.Severity), c.Field, c.Old.String(), c.New.String())
			}
		}
	}
	return nil
}

func events(args []string) error {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	since := fs.String("since", "1h", "how far back")
	node := fs.String("node", "", "filter by node")
	ns := fs.String("namespace", "", "filter by namespace")
	_ = fs.Parse(args)
	var evs []struct {
		Type, Reason, Namespace, ObjName, Node, Message string
		Count                                           int
		Last                                            time.Time
	}
	if err := get(fmt.Sprintf("/api/v1/events?from=-%s&node=%s&namespace=%s", *since, *node, *ns), &evs); err != nil {
		return err
	}
	w := table()
	fmt.Fprintln(w, "LAST\tTYPE\tREASON\tNODE\tOBJECT\tCOUNT\tMESSAGE")
	for _, e := range evs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s/%s\t%d\t%s\n", e.Last.Local().Format("15:04:05"), e.Type, e.Reason, e.Node, e.Namespace, e.ObjName, e.Count, trunc(e.Message, 60))
	}
	return w.Flush()
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
