package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/USA-RedDragon/b2-backup-operator/internal/borgbase"
)

type fakeAPI struct {
	mu    sync.Mutex
	repos map[string]*borgbase.Repo

	nextID int

	calls []string

	edits []borgbase.EditOptions

	getErr    error
	deleteErr error
}

func newFakeAPI(repos ...*borgbase.Repo) *fakeAPI {
	f := &fakeAPI{repos: map[string]*borgbase.Repo{}}
	for _, r := range repos {
		f.repos[r.ID] = r
	}
	return f
}

func (f *fakeAPI) record(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
}

func (f *fakeAPI) called(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, op)
}

func (f *fakeAPI) countCalls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == op {
			n++
		}
	}
	return n
}

func copyRepo(r *borgbase.Repo) *borgbase.Repo {
	cp := *r
	return &cp
}

func (f *fakeAPI) Get(_ context.Context, id string) (*borgbase.Repo, error) {
	f.record("Get")
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.repos[id]; ok {
		return copyRepo(r), nil
	}
	return nil, fmt.Errorf("%w: %s", borgbase.ErrNotFound, id)
}

func (f *fakeAPI) FindByName(_ context.Context, name string) (*borgbase.Repo, error) {
	f.record("FindByName")
	f.mu.Lock()
	defer f.mu.Unlock()

	var found *borgbase.Repo
	for _, r := range f.repos {
		if r.Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf(
				"borgbase: multiple repositories named %q; adopt one explicitly by ID", name)
		}
		found = r
	}
	if found == nil {
		return nil, fmt.Errorf("%w: name %s", borgbase.ErrNotFound, name)
	}
	return copyRepo(found), nil
}

func (f *fakeAPI) Add(_ context.Context, opts borgbase.AddOptions) (*borgbase.Repo, error) {
	f.record("Add")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("new%04d", f.nextID)
	r := &borgbase.Repo{
		ID:           id,
		Name:         opts.Name,
		Region:       opts.Region,
		Format:       borgbase.FormatRestic,
		Htpasswd:     "token-" + id,
		QuotaEnabled: opts.Quota != nil,
		AppendOnly:   opts.AppendOnly,
	}
	if opts.Quota != nil {
		r.Quota = *opts.Quota
	}
	if opts.AlertDays != nil {
		r.AlertDays = *opts.AlertDays
	}
	f.repos[id] = r
	return copyRepo(r), nil
}

func (f *fakeAPI) Edit(_ context.Context, id string, opts borgbase.EditOptions) (*borgbase.Repo, error) {
	f.record("Edit")
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", borgbase.ErrNotFound, id)
	}
	f.edits = append(f.edits, opts)
	if opts.QuotaEnabled != nil {
		r.QuotaEnabled = *opts.QuotaEnabled
	}
	if opts.Quota != nil {
		r.Quota = *opts.Quota
	}
	if opts.AlertDays != nil {
		r.AlertDays = *opts.AlertDays
	}
	if opts.AppendOnly != nil {
		r.AppendOnly = *opts.AppendOnly
	}
	return copyRepo(r), nil
}

func (f *fakeAPI) lastEdit() borgbase.EditOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) == 0 {
		return borgbase.EditOptions{}
	}
	return f.edits[len(f.edits)-1]
}

func (f *fakeAPI) Delete(_ context.Context, id string) error {
	f.record("Delete")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.repos[id]; !ok {
		return fmt.Errorf("%w: %s", borgbase.ErrNotFound, id)
	}
	delete(f.repos, id)
	return nil
}
