package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

const unrecognizedOperation = "unrecognized GraphQL operation"

type repo struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Region       string  `json:"region"`
	Format       string  `json:"format"`
	RepoPath     string  `json:"repoPath"`
	Quota        int64   `json:"quota"`
	QuotaEnabled bool    `json:"quotaEnabled"`
	AlertDays    int64   `json:"alertDays"`
	AppendOnly   bool    `json:"appendOnly"`
	CurrentUsage float64 `json:"currentUsage"`
	Htpasswd     string  `json:"htpasswd"`
	Server       struct {
		Hostname string `json:"hostname"`
		Region   string `json:"region"`
	} `json:"server"`
}

type store struct {
	mu    sync.Mutex
	repos map[string]*repo
	next  int
}

type request struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

func main() {
	s := &store{repos: map[string]*repo{}}
	http.HandleFunc("/graphql", s.handle)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("borgbase stub listening on %s", addr)
	server := &http.Server{Addr: addr}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func (s *store) handle(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "borgbase stub: malformed request")
		return
	}

	op := req.OperationName
	if op == "" {
		op = operationName(req.Query)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch op {
	case "repoList":
		name, _ := req.Variables["name"].(string)
		var found []*repo
		for _, r := range s.repos {
			if r.Name == name {
				found = append(found, r)
			}
		}
		writeData(w, map[string]any{"repoList": found})

	case "repo":
		id, _ := req.Variables["id"].(string)
		r, ok := s.repos[id]
		if !ok {
			writeErr(w, "Repository not found")
			return
		}
		writeData(w, map[string]any{"repo": r})

	case "repoAdd":
		s.next++
		id := fmt.Sprintf("stub%04d", s.next)
		r := &repo{
			ID:       id,
			Name:     str(req.Variables["name"]),
			Region:   str(req.Variables["region"]),
			Format:   str(req.Variables["format"]),
			Htpasswd: "stub-password-" + id,
		}
		r.Server.Hostname = id + ".repo.borgbase.com"
		apply(r, req.Variables)
		s.repos[id] = r
		writeData(w, map[string]any{"repoAdd": map[string]any{"repoAdded": r}})

	case "repoEdit":
		id := str(req.Variables["id"])
		r, ok := s.repos[id]
		if !ok {
			writeErr(w, "Repository not found")
			return
		}
		apply(r, req.Variables)
		writeData(w, map[string]any{"repoEdit": map[string]any{"repoEdited": r}})

	case "repoDelete":
		delete(s.repos, str(req.Variables["id"]))
		writeData(w, map[string]any{"repoDelete": map[string]any{"ok": true}})

	default:
		log.Printf("%s %q; query was: %s", unrecognizedOperation, op, strings.Join(strings.Fields(req.Query), " "))
		writeErr(w, fmt.Sprintf("borgbase stub: %s %q; the stub in test/e2e/stub has drifted "+
			"from the queries in internal/borgbase", unrecognizedOperation, op))
	}
}

func operationName(query string) string {
	tok, i := nextToken(query, 0)
	switch tok {
	case "query", "mutation", "subscription":
		tok, i = nextToken(query, i)
	}
	if isNameToken(tok) {
		return tok
	}

	for tok != "" {
		if tok == "{" {
			tok, _ = nextToken(query, i)
			if isNameToken(tok) {
				return tok
			}
			return ""
		}
		tok, i = nextToken(query, i)
	}
	return ""
}

func nextToken(s string, i int) (string, int) {
	for i < len(s) {
		switch c := s[i]; {
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',':
			i++
		case isNameByte(c):
			j := i
			for j < len(s) && isNameByte(s[j]) {
				j++
			}
			return s[i:j], j
		default:
			return s[i : i+1], i + 1
		}
	}
	return "", i
}

func isNameByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isNameToken(tok string) bool {
	return tok != "" && isNameByte(tok[0]) && (tok[0] < '0' || tok[0] > '9')
}

func apply(r *repo, vars map[string]any) {
	if v, ok := vars["quota"].(float64); ok {
		r.Quota = int64(v)
	}
	if v, ok := vars["quotaEnabled"].(bool); ok {
		r.QuotaEnabled = v
	}
	if v, ok := vars["alertDays"].(float64); ok {
		r.AlertDays = int64(v)
	}
	if v, ok := vars["appendOnly"].(bool); ok {
		r.AppendOnly = v
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func writeData(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeErr(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"message": message}},
	})
}
