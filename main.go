// Context-graph scorecard API.
//
// On start: load services/*.yaml into Redis as nodes + edges, then serve:
//   GET /health
//   GET /services
//   GET /scorecard?service=<name>   (omit service = all)
//   GET /why_not_ready?service=<name>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

const readyThreshold = 80

// Weighted rule checks — total 100. Every check is explainable on camera.
var weights = map[string]int{
	"helm_chart":        15,
	"argocd_status":     20,
	"terraform_managed": 15,
	"ci_status":         25,
	"owner":             15,
	"ai_agent_usage":    10,
}

type Manifest struct {
	Name              string `yaml:"name" json:"name"`
	HelmChart         string `yaml:"helm_chart" json:"helm_chart"`
	ArgoCDStatus      string `yaml:"argocd_status" json:"argocd_status"`
	TerraformManaged  bool   `yaml:"terraform_managed" json:"terraform_managed"`
	CIStatus          string `yaml:"ci_status" json:"ci_status"`
	Owner             string `yaml:"owner" json:"owner"`
	AIAgentUsage      string `yaml:"ai_agent_usage" json:"ai_agent_usage"`
	Repo              string `yaml:"repo" json:"repo"`
}

type Check struct {
	Rule    string `json:"rule"`
	Points  int    `json:"points"`
	Max     int    `json:"max"`
	Pass    bool   `json:"pass"`
	Detail  string `json:"detail"`
}

type Scorecard struct {
	Service string  `json:"service"`
	Score   int     `json:"score"`
	Ready   bool    `json:"ready"`
	Checks  []Check `json:"checks"`
	Gaps    []string `json:"gaps"`
	Owner   string  `json:"owner"`
	Repo    string  `json:"repo"`
}

type server struct {
	rdb *redis.Client
}

func main() {
	redisAddr := env("REDIS_ADDR", "127.0.0.1:6381")
	listen := env("LISTEN", ":8091")
	manifestDir := env("MANIFEST_DIR", "services")

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis %s: %v", redisAddr, err)
	}

	s := &server{rdb: rdb}
	n, err := s.loadManifests(ctx, manifestDir)
	if err != nil {
		log.Fatalf("load manifests: %v", err)
	}
	log.Printf("loaded %d services from %s into redis graph", n, manifestDir)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/services", s.handleServices)
	mux.HandleFunc("/scorecard", s.handleScorecard)
	mux.HandleFunc("/why_not_ready", s.handleWhyNotReady)

	log.Printf("scorecard api listening on %s", listen)
	log.Fatal(http.ListenAndServe(listen, mux))
}

// --- Redis graph load -------------------------------------------------------

func (s *server) loadManifests(ctx context.Context, dir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no *.yaml in %s", dir)
	}

	// Fresh demo graph each boot.
	if err := s.rdb.FlushDB(ctx).Err(); err != nil {
		return 0, err
	}

	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		var m Manifest
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		if m.Name == "" {
			return 0, fmt.Errorf("%s: missing name", path)
		}
		if err := s.writeNode(ctx, m); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}

// writeNode stores the service hash plus three edge keys the idea asked for:
// service→owner, service→infra, service→repo.
func (s *server) writeNode(ctx context.Context, m Manifest) error {
	key := "svc:" + m.Name
	pipe := s.rdb.TxPipeline()
	pipe.SAdd(ctx, "services", m.Name)
	pipe.HSet(ctx, key, map[string]interface{}{
		"name":               m.Name,
		"helm_chart":         m.HelmChart,
		"argocd_status":      m.ArgoCDStatus,
		"terraform_managed":  strconv.FormatBool(m.TerraformManaged),
		"ci_status":          m.CIStatus,
		"owner":              m.Owner,
		"ai_agent_usage":     m.AIAgentUsage,
		"repo":               m.Repo,
	})
	pipe.Set(ctx, "edge:owner:"+m.Name, m.Owner, 0)
	pipe.Del(ctx, "edge:infra:"+m.Name)
	pipe.SAdd(ctx, "edge:infra:"+m.Name,
		"helm:"+nullDash(m.HelmChart),
		"argocd:"+nullDash(m.ArgoCDStatus),
		"terraform:"+strconv.FormatBool(m.TerraformManaged),
	)
	pipe.Set(ctx, "edge:repo:"+m.Name, m.Repo, 0)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *server) readManifest(ctx context.Context, name string) (Manifest, error) {
	vals, err := s.rdb.HGetAll(ctx, "svc:"+name).Result()
	if err != nil {
		return Manifest{}, err
	}
	if len(vals) == 0 {
		return Manifest{}, fmt.Errorf("unknown service %q", name)
	}
	tf, _ := strconv.ParseBool(vals["terraform_managed"])
	return Manifest{
		Name:             vals["name"],
		HelmChart:        vals["helm_chart"],
		ArgoCDStatus:     vals["argocd_status"],
		TerraformManaged: tf,
		CIStatus:         vals["ci_status"],
		Owner:            vals["owner"],
		AIAgentUsage:     vals["ai_agent_usage"],
		Repo:             vals["repo"],
	}, nil
}

// --- Scoring ----------------------------------------------------------------

func score(m Manifest) Scorecard {
	checks := []Check{
		checkHelm(m),
		checkArgo(m),
		checkTerraform(m),
		checkCI(m),
		checkOwner(m),
		checkAI(m),
	}
	total := 0
	gaps := []string{}
	for _, c := range checks {
		total += c.Points
		if !c.Pass {
			gaps = append(gaps, c.Detail)
		}
	}
	return Scorecard{
		Service: m.Name,
		Score:   total,
		Ready:   total >= readyThreshold,
		Checks:  checks,
		Gaps:    gaps,
		Owner:   m.Owner,
		Repo:    m.Repo,
	}
}

func checkHelm(m Manifest) Check {
	max := weights["helm_chart"]
	ok := strings.TrimSpace(m.HelmChart) != ""
	pts, detail := 0, "no Helm chart registered"
	if ok {
		pts, detail = max, "helm_chart="+m.HelmChart
	}
	return Check{Rule: "helm_chart", Points: pts, Max: max, Pass: ok, Detail: detail}
}

func checkArgo(m Manifest) Check {
	max := weights["argocd_status"]
	st := strings.ToLower(strings.TrimSpace(m.ArgoCDStatus))
	pts, ok, detail := 0, false, "argocd_status="+nullDash(m.ArgoCDStatus)
	switch st {
	case "healthy", "synced":
		pts, ok = max, true
	case "degraded":
		pts = max / 2
		detail += " (partial credit)"
	default:
		detail += " (want Healthy or Synced)"
	}
	return Check{Rule: "argocd_status", Points: pts, Max: max, Pass: ok, Detail: detail}
}

func checkTerraform(m Manifest) Check {
	max := weights["terraform_managed"]
	ok := m.TerraformManaged
	pts, detail := 0, "not terraform-managed"
	if ok {
		pts, detail = max, "terraform_managed=true"
	}
	return Check{Rule: "terraform_managed", Points: pts, Max: max, Pass: ok, Detail: detail}
}

func checkCI(m Manifest) Check {
	max := weights["ci_status"]
	st := strings.ToLower(strings.TrimSpace(m.CIStatus))
	pts, ok, detail := 0, false, "ci_status="+nullDash(m.CIStatus)
	switch st {
	case "passing":
		pts, ok = max, true
	case "pending":
		pts = 10
		detail += " (partial credit)"
	default:
		detail += " (want passing)"
	}
	return Check{Rule: "ci_status", Points: pts, Max: max, Pass: ok, Detail: detail}
}

func checkOwner(m Manifest) Check {
	max := weights["owner"]
	ok := strings.TrimSpace(m.Owner) != ""
	pts, detail := 0, "no owner on the service"
	if ok {
		pts, detail = max, "owner="+m.Owner
	}
	return Check{Rule: "owner", Points: pts, Max: max, Pass: ok, Detail: detail}
}

func checkAI(m Manifest) Check {
	max := weights["ai_agent_usage"]
	v := strings.TrimSpace(m.AIAgentUsage)
	ok := v != "" && !strings.EqualFold(v, "none")
	pts, detail := 0, "ai_agent_usage unset/none"
	if ok {
		pts, detail = max, "ai_agent_usage="+v
	}
	return Check{Rule: "ai_agent_usage", Points: pts, Max: max, Pass: ok, Detail: detail}
}

// --- HTTP handlers ----------------------------------------------------------

func (s *server) handleServices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	names, err := s.rdb.SMembers(ctx, "services").Result()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		m, err := s.readManifest(ctx, name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		owner, _ := s.rdb.Get(ctx, "edge:owner:"+name).Result()
		repo, _ := s.rdb.Get(ctx, "edge:repo:"+name).Result()
		infra, _ := s.rdb.SMembers(ctx, "edge:infra:"+name).Result()
		out = append(out, map[string]any{
			"name":  m.Name,
			"owner": owner,
			"repo":  repo,
			"infra": infra,
			"raw":   m,
		})
	}
	writeJSON(w, map[string]any{"services": out, "count": len(out)})
}

func (s *server) handleScorecard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svc := r.URL.Query().Get("service")
	names, err := s.serviceNames(ctx, svc)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	cards := make([]Scorecard, 0, len(names))
	for _, name := range names {
		m, err := s.readManifest(ctx, name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		cards = append(cards, score(m))
	}
	if svc != "" {
		writeJSON(w, cards[0])
		return
	}
	writeJSON(w, map[string]any{"scorecards": cards, "ready_threshold": readyThreshold})
}

func (s *server) handleWhyNotReady(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svc := r.URL.Query().Get("service")
	names, err := s.serviceNames(ctx, svc)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	type item struct {
		Service string   `json:"service"`
		Score   int      `json:"score"`
		Ready   bool     `json:"ready"`
		Why     []string `json:"why"`
		Remediation string `json:"remediation"`
	}
	var items []item
	for _, name := range names {
		m, err := s.readManifest(ctx, name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		sc := score(m)
		if sc.Ready && svc == "" {
			continue // list only not-ready unless a single service was asked
		}
		items = append(items, item{
			Service:     sc.Service,
			Score:       sc.Score,
			Ready:       sc.Ready,
			Why:         sc.Gaps,
			Remediation: remediate(sc),
		})
	}
	writeJSON(w, map[string]any{
		"not_ready":       items,
		"ready_threshold": readyThreshold,
		"count":           len(items),
	})
}

func remediate(sc Scorecard) string {
	if sc.Ready {
		return "service meets the ready threshold"
	}
	parts := make([]string, 0, len(sc.Gaps))
	for _, g := range sc.Gaps {
		parts = append(parts, "- "+g)
	}
	return fmt.Sprintf("%s scores %d/%d. Fix:\n%s", sc.Service, sc.Score, 100, strings.Join(parts, "\n"))
}

func (s *server) serviceNames(ctx context.Context, one string) ([]string, error) {
	if one != "" {
		exists, err := s.rdb.SIsMember(ctx, "services", one).Result()
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("unknown service %q", one)
		}
		return []string{one}, nil
	}
	return s.rdb.SMembers(ctx, "services").Result()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func nullDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
