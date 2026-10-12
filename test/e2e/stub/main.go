package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
)

func main() {
	f := b2fake.New(env("MASTER_KEY_ID", "stub-master-key-id"), env("MASTER_KEY", "stub-master-key"))
	f.S3APIURL = env("S3_API_URL", f.S3APIURL)

	for name := range strings.FieldsSeq(os.Getenv("BUCKETS")) {
		f.AddBucket(name)
	}
	for spec := range strings.FieldsSeq(os.Getenv("FILES")) {
		bucketName, file, ok := strings.Cut(spec, ":")
		bucket := f.Bucket(bucketName)
		if !ok || bucket == nil {
			log.Fatalf("FILES entry %q does not name a bucket listed in BUCKETS", spec)
		}
		f.PutFile(bucket.BucketID, file, 155)
	}

	var api http.Handler = f
	if dir := os.Getenv("MIRROR_DIR"); dir != "" {
		m := newMirror(dir, f)
		api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.sync()
			f.ServeHTTP(w, r)
		})
	}

	mux := http.NewServeMux()
	mux.Handle("/b2api/", api)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := env("ADDR", ":8080")
	log.Printf("b2 stub listening on %s", addr)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
