package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
)

// mirror reflects the files an S3 server keeps under dir, one directory per
// bucket, into the fake account, so B2 calls such as usage see what restic
// actually wrote. A new or resized file is uploaded and a removed one hidden,
// as B2 does for S3 writes and deletes.
type mirror struct {
	dir  string
	fake *b2fake.Server

	mu   sync.Mutex
	seen map[string]int64
}

func newMirror(dir string, fake *b2fake.Server) *mirror {
	return &mirror{dir: dir, fake: fake, seen: map[string]int64{}}
}

func (m *mirror) sync() {
	m.mu.Lock()
	defer m.mu.Unlock()

	buckets, err := os.ReadDir(m.dir)
	if err != nil {
		log.Printf("mirror: reading %s: %v", m.dir, err)
		return
	}
	current := map[string]int64{}
	ids := map[string]string{}
	for _, entry := range buckets {
		if !entry.IsDir() {
			continue
		}
		bucket := m.fake.Bucket(entry.Name())
		if bucket == nil {
			continue
		}
		ids[entry.Name()] = bucket.BucketID
		root := filepath.Join(m.dir, entry.Name())
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, ".partial") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			current[entry.Name()+"/"+filepath.ToSlash(rel)] = info.Size()
			return nil
		})
	}
	for key, size := range current {
		if old, ok := m.seen[key]; ok && old == size {
			continue
		}
		bucket, name, _ := strings.Cut(key, "/")
		m.fake.PutFile(ids[bucket], name, size)
	}
	for key := range m.seen {
		if _, ok := current[key]; ok {
			continue
		}
		bucket, name, _ := strings.Cut(key, "/")
		if id, ok := ids[bucket]; ok {
			m.fake.HideFile(id, name)
		}
	}
	m.seen = current
}
