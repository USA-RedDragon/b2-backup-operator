// Package b2fake is an in-memory stand-in for the management calls of the
// Backblaze B2 native API, shared by the unit tests and the e2e stub.
package b2fake

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
)

// AccountID is the account a new Server's buckets and keys belong to.
const AccountID = "fakeaccount"

// UnrecognizedOperation is logged and returned for any call the fake does not implement.
const UnrecognizedOperation = "unrecognized B2 operation"

const (
	actionStart  = b2.ActionStart
	actionUpload = b2.ActionUpload
	actionHide   = b2.ActionHide
	fieldFiles   = "files"
)

// Failure makes the next Times calls to an operation fail.
type Failure struct {
	Status     int
	Code       string
	Message    string
	RetryAfter string
	Times      int
}

// Server is a fake B2 account.
type Server struct {
	S3APIURL string
	Logf     func(format string, args ...any)

	account string

	mu       sync.Mutex
	next     int
	keys     map[string]*keyRecord
	buckets  map[string]*b2.Bucket
	versions []version
	tokens   map[string]string
	calls    []string
	failures map[string][]Failure
}

type keyRecord struct {
	b2.Key
	secret string
}

type version struct {
	bucketID string
	file     b2.File
}

// New returns a fake account, AccountID, with one master key.
func New(masterKeyID, masterKey string) *Server {
	return NewAccount(AccountID, masterKeyID, masterKey)
}

// NewAccount returns a fake account with the given ID and one master key.
func NewAccount(accountID, masterKeyID, masterKey string) *Server {
	s := &Server{
		S3APIURL: "https://s3.us-east-005.backblazeb2.com",
		account:  accountID,
		keys:     map[string]*keyRecord{},
		buckets:  map[string]*b2.Bucket{},
		tokens:   map[string]string{},
		failures: map[string][]Failure{},
	}
	s.keys[masterKeyID] = &keyRecord{
		AccountID: s.account, ApplicationKeyID: masterKeyID, KeyName: "master",
		Capabilities: []string{
			"listBuckets", "writeBuckets", "deleteBuckets", "listKeys", "writeKeys", "deleteKeys",
			"listFiles", "readFiles", "writeFiles", "deleteFiles",
		},
		secret: masterKey,
	}
	return s
}

// AddBucket creates a bucket directly, as if someone had made it by hand.
func (s *Server) AddBucket(name string, rules ...b2.LifecycleRule) *b2.Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addBucket(name, rules)
}

func (s *Server) addBucket(name string, rules []b2.LifecycleRule) *b2.Bucket {
	s.next++
	if rules == nil {
		rules = []b2.LifecycleRule{}
	}
	bucket := &b2.Bucket{
		AccountID: s.account, BucketID: fmt.Sprintf("bucket%04d", s.next), BucketName: name,
		BucketType: b2.BucketTypePrivate, LifecycleRules: rules, Revision: 1,
	}
	s.buckets[bucket.BucketID] = bucket
	return bucket
}

// AddKey creates an application key directly and returns its ID.
func (s *Server) AddKey(name, bucketID, namePrefix string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addKey(name, bucketID, namePrefix, nil).ApplicationKeyID
}

func (s *Server) addKey(name, bucketID, namePrefix string, caps []string) *keyRecord {
	s.next++
	id := fmt.Sprintf("key%04d", s.next)
	rec := &keyRecord{
		AccountID: s.account, ApplicationKeyID: id, KeyName: name, Capabilities: caps,
		BucketID: bucketID, NamePrefix: namePrefix,
		secret: "secret-" + id,
	}
	s.keys[id] = rec
	return rec
}

// PutFile uploads a file version and returns its ID.
func (s *Server) PutFile(bucketID, name string, size int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putFile(bucketID, name, size, actionUpload)
}

// HideFile adds a hide marker for name, as an S3 DeleteObject does on B2.
func (s *Server) HideFile(bucketID, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putFile(bucketID, name, 0, actionHide)
}

// StartLargeFile records an unfinished large file upload and returns its ID.
func (s *Server) StartLargeFile(bucketID, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putFile(bucketID, name, 0, actionStart)
}

func (s *Server) putFile(bucketID, name string, size int64, action string) string {
	s.next++
	id := fmt.Sprintf("file%06d", s.next)
	s.versions = append(s.versions, version{bucketID: bucketID, file: b2.File{
		FileID: id, FileName: name, Action: action, ContentLength: size,
	}})
	return id
}

// Bucket returns a copy of the bucket with the given name, or nil.
func (s *Server) Bucket(name string) *b2.Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.buckets {
		if b.BucketName == name {
			cp := *b
			cp.LifecycleRules = slices.Clone(b.LifecycleRules)
			return &cp
		}
	}
	return nil
}

// Keys returns every application key except the master key.
func (s *Server) Keys() []b2.Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []b2.Key
	for _, k := range s.keys {
		if k.KeyName == "master" {
			continue
		}
		out = append(out, k.Key)
	}
	slices.SortFunc(out, func(a, b b2.Key) int { return strings.Compare(a.ApplicationKeyID, b.ApplicationKeyID) })
	return out
}

// KeySecret returns the secret half of an application key.
func (s *Server) KeySecret(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[id]; ok {
		return k.secret
	}
	return ""
}

// DeleteKeyDirectly removes a key as if someone had revoked it by hand.
func (s *Server) DeleteKeyDirectly(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, id)
}

// FileNames returns the names of every version in the bucket, in upload order.
func (s *Server) FileNames(bucketID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, v := range s.versions {
		if v.bucketID == bucketID {
			out = append(out, v.file.FileName)
		}
	}
	return out
}

// Calls returns the operations called so far, in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// CountCalls returns how many times op was called.
func (s *Server) CountCalls(op string) int {
	n := 0
	for _, c := range s.Calls() {
		if c == op {
			n++
		}
	}
	return n
}

// Fail queues a failure for op.
func (s *Server) Fail(op string, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Times == 0 {
		f.Times = 1
	}
	s.failures[op] = append(s.failures[op], f)
}

// ExpireTokens invalidates every authorization token handed out so far.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.tokens {
		s.tokens[t] = ""
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// ServeHTTP implements the API under /b2api/v3/.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op, ok := strings.CutPrefix(r.URL.Path, "/b2api/v3/")
	if !ok {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, op)

	if f, ok := s.popFailure(op); ok {
		if f.RetryAfter != "" {
			w.Header().Set("Retry-After", f.RetryAfter)
		}
		writeError(w, f.Status, f.Code, f.Message)
		return
	}

	if op == "b2_authorize_account" {
		s.authorize(w, r)
		return
	}

	keyID, valid := s.tokens[r.Header.Get("Authorization")]
	switch {
	case !valid:
		writeError(w, http.StatusUnauthorized, b2.CodeBadAuthToken, "invalid authorization token")
		return
	case keyID == "":
		writeError(w, http.StatusUnauthorized, b2.CodeExpiredAuthToken, "authorization token has expired")
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, b2.CodeBadRequest, "malformed JSON")
		return
	}

	handler, known := s.handlers()[op]
	if !known {
		s.logf("%s %q", UnrecognizedOperation, op)
		writeError(w, http.StatusBadRequest, b2.CodeBadRequest,
			fmt.Sprintf("b2fake: %s %q; teach internal/b2/b2fake about it", UnrecognizedOperation, op))
		return
	}
	handler(w, body)
}

func (s *Server) popFailure(op string) (Failure, bool) {
	queue := s.failures[op]
	if len(queue) == 0 {
		return Failure{}, false
	}
	f := queue[0]
	f.Times--
	if f.Times <= 0 {
		s.failures[op] = queue[1:]
	} else {
		queue[0] = f
	}
	return f, true
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
		if raw, err := base64.StdEncoding.DecodeString(auth); err == nil {
			id, secret, ok = strings.Cut(string(raw), ":")
		}
	}
	key, known := s.keys[id]
	if !ok || !known || key.secret != secret {
		writeError(w, http.StatusUnauthorized, b2.CodeUnauthorized, "invalid application key")
		return
	}
	s.next++
	token := fmt.Sprintf("token%06d", s.next)
	s.tokens[token] = id

	var bucketID any
	if key.BucketID != "" {
		bucketID = key.BucketID
	}
	var namePrefix any
	if key.NamePrefix != "" {
		namePrefix = key.NamePrefix
	}
	writeJSON(w, map[string]any{
		"accountId":          s.account,
		"authorizationToken": token,
		"apiInfo": map[string]any{
			"storageApi": map[string]any{
				"apiUrl":       "http://" + r.Host,
				"downloadUrl":  "http://" + r.Host,
				"s3ApiUrl":     s.S3APIURL,
				"bucketId":     bucketID,
				"namePrefix":   namePrefix,
				"capabilities": key.Capabilities,
				"infoType":     "storageApi",
			},
		},
	})
}

type handlerFunc func(w http.ResponseWriter, body map[string]any)

func (s *Server) handlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"b2_list_buckets":                s.listBuckets,
		"b2_create_bucket":               s.createBucket,
		"b2_update_bucket":               s.updateBucket,
		"b2_delete_bucket":               s.deleteBucket,
		"b2_create_key":                  s.createKey,
		"b2_list_keys":                   s.listKeys,
		"b2_delete_key":                  s.deleteKey,
		"b2_list_file_names":             s.listFileNames,
		"b2_list_file_versions":          s.listFileVersions,
		"b2_delete_file_version":         s.deleteFileVersion,
		"b2_list_unfinished_large_files": s.listUnfinished,
		"b2_cancel_large_file":           s.cancelLargeFile,
	}
}

func (s *Server) listBuckets(w http.ResponseWriter, body map[string]any) {
	name := str(body["bucketName"])
	id := str(body["bucketId"])
	out := []b2.Bucket{}
	for _, b := range s.buckets {
		if (name == "" || b.BucketName == name) && (id == "" || b.BucketID == id) {
			out = append(out, *b)
		}
	}
	slices.SortFunc(out, func(a, b b2.Bucket) int { return strings.Compare(a.BucketName, b.BucketName) })
	writeJSON(w, map[string]any{"buckets": out})
}

func (s *Server) createBucket(w http.ResponseWriter, body map[string]any) {
	name := str(body["bucketName"])
	for _, b := range s.buckets {
		if b.BucketName == name {
			writeError(w, http.StatusBadRequest, b2.CodeDuplicateBucketName, "Bucket name is already in use.")
			return
		}
	}
	if len(name) < 6 || len(name) > 63 || strings.HasPrefix(name, "b2-") {
		writeError(w, http.StatusBadRequest, b2.CodeBadRequest, "Invalid bucket name")
		return
	}
	rules, err := decodeRules(body["lifecycleRules"])
	if err != nil {
		writeError(w, http.StatusBadRequest, b2.CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, s.addBucket(name, rules))
}

func (s *Server) updateBucket(w http.ResponseWriter, body map[string]any) {
	b, ok := s.buckets[str(body["bucketId"])]
	if !ok {
		writeError(w, http.StatusBadRequest, b2.CodeBadBucketID, "Invalid bucketId")
		return
	}
	if rev, ok := body["ifRevisionIs"].(float64); ok && int(rev) != b.Revision {
		writeError(w, http.StatusConflict, b2.CodeConflict, "The ifRevisionIs test failed.")
		return
	}
	if raw, ok := body["lifecycleRules"]; ok {
		rules, err := decodeRules(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, b2.CodeBadRequest, err.Error())
			return
		}
		b.LifecycleRules = rules
	}
	b.Revision++
	writeJSON(w, b)
}

func decodeRules(raw any) ([]b2.LifecycleRule, error) {
	rules := []b2.LifecycleRule{}
	if raw == nil {
		return rules, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, err
	}
	if len(rules) > 100 {
		return nil, fmt.Errorf("a bucket can have at most 100 lifecycle rules")
	}
	for i, a := range rules {
		if a.DaysFromHidingToDeleting == nil && a.DaysFromUploadingToHiding == nil &&
			a.DaysFromStartingToCancelingUnfinishedLargeFiles == nil {
			return nil, fmt.Errorf("lifecycle rule %q sets no days", a.FileNamePrefix)
		}
		for j, b := range rules {
			if i != j && a.FileNamePrefix == b.FileNamePrefix {
				return nil, fmt.Errorf("two lifecycle rules for %q", a.FileNamePrefix)
			}
		}
	}
	return rules, nil
}

func (s *Server) deleteBucket(w http.ResponseWriter, body map[string]any) {
	id := str(body["bucketId"])
	b, ok := s.buckets[id]
	if !ok {
		writeError(w, http.StatusBadRequest, b2.CodeBadBucketID, "Invalid bucketId")
		return
	}
	for _, v := range s.versions {
		if v.bucketID == id {
			writeError(w, http.StatusBadRequest, b2.CodeCannotDeleteBucket, "Cannot delete non-empty bucket")
			return
		}
	}
	delete(s.buckets, id)
	writeJSON(w, b)
}

func (s *Server) createKey(w http.ResponseWriter, body map[string]any) {
	name := str(body["keyName"])
	if name == "" || len(name) > 100 || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		writeError(w, http.StatusBadRequest, b2.CodeBadRequest, "Invalid keyName")
		return
	}
	var caps []string
	if list, ok := body["capabilities"].([]any); ok {
		for _, c := range list {
			caps = append(caps, str(c))
		}
	}
	bucketID := str(body["bucketId"])
	if bucketID != "" {
		if _, ok := s.buckets[bucketID]; !ok {
			writeError(w, http.StatusBadRequest, b2.CodeBadRequest, "Invalid bucketId")
			return
		}
	}
	rec := s.addKey(name, bucketID, str(body["namePrefix"]), caps)
	out := rec.Key
	out.ApplicationKey = rec.secret
	writeJSON(w, out)
}

func (s *Server) listKeys(w http.ResponseWriter, body map[string]any) {
	limit := 1000
	if n, ok := body["maxKeyCount"].(float64); ok && n > 0 {
		limit = int(n)
	}
	start := str(body["startApplicationKeyId"])
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		if id >= start {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var next any
	if len(ids) > limit {
		next = ids[limit]
		ids = ids[:limit]
	}
	keys := make([]b2.Key, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, s.keys[id].Key)
	}
	writeJSON(w, map[string]any{"keys": keys, "nextApplicationKeyId": next})
}

func (s *Server) deleteKey(w http.ResponseWriter, body map[string]any) {
	id := str(body["applicationKeyId"])
	k, ok := s.keys[id]
	if !ok {
		writeError(w, http.StatusBadRequest, b2.CodeBadRequest, "Key not found: "+id)
		return
	}
	delete(s.keys, id)
	for token, owner := range s.tokens {
		if owner == id {
			delete(s.tokens, token)
		}
	}
	writeJSON(w, k.Key)
}

func (s *Server) sortedVersions(bucketID, prefix string) []b2.File {
	var out []b2.File
	for _, v := range slices.Backward(s.versions) {
		if v.bucketID == bucketID && strings.HasPrefix(v.file.FileName, prefix) {
			out = append(out, v.file)
		}
	}
	slices.SortStableFunc(out, func(a, b b2.File) int { return strings.Compare(a.FileName, b.FileName) })
	return out
}

func pageSize(body map[string]any, def, limit int) int {
	n := def
	if v, ok := body["maxFileCount"].(float64); ok && v > 0 {
		n = int(v)
	}
	return min(n, limit)
}

func (s *Server) listFileNames(w http.ResponseWriter, body map[string]any) {
	if _, ok := s.buckets[str(body["bucketId"])]; !ok {
		writeError(w, http.StatusBadRequest, b2.CodeBadBucketID, "Invalid bucketId")
		return
	}
	start := str(body["startFileName"])
	size := pageSize(body, 100, b2.MaxFileCount)
	var latest []b2.File
	seen := map[string]bool{}
	for _, f := range s.sortedVersions(str(body["bucketId"]), str(body["prefix"])) {
		if f.Action == actionStart || seen[f.FileName] {
			continue
		}
		seen[f.FileName] = true
		if f.Action == actionUpload && f.FileName >= start {
			latest = append(latest, f)
		}
	}
	var next any
	if len(latest) > size {
		next = latest[size].FileName
		latest = latest[:size]
	}
	if latest == nil {
		latest = []b2.File{}
	}
	writeJSON(w, map[string]any{fieldFiles: latest, "nextFileName": next})
}

func (s *Server) listFileVersions(w http.ResponseWriter, body map[string]any) {
	if _, ok := s.buckets[str(body["bucketId"])]; !ok {
		writeError(w, http.StatusBadRequest, b2.CodeBadBucketID, "Invalid bucketId")
		return
	}
	start := str(body["startFileName"])
	if start == "" && str(body["startFileId"]) != "" {
		writeError(w, http.StatusBadRequest, b2.CodeBadRequest, "startFileId requires startFileName")
		return
	}
	size := pageSize(body, 100, b2.MaxFileCount)
	var all []b2.File
	for _, f := range s.sortedVersions(str(body["bucketId"]), str(body["prefix"])) {
		if f.FileName >= start {
			all = append(all, f)
		}
	}
	var nextName, nextID any
	if len(all) > size {
		nextName, nextID = all[size].FileName, all[size].FileID
		all = all[:size]
	}
	if all == nil {
		all = []b2.File{}
	}
	writeJSON(w, map[string]any{fieldFiles: all, "nextFileName": nextName, "nextFileId": nextID})
}

func (s *Server) deleteFileVersion(w http.ResponseWriter, body map[string]any) {
	name, id := str(body["fileName"]), str(body["fileId"])
	for i, v := range s.versions {
		if v.file.FileID == id && v.file.FileName == name {
			s.versions = slices.Delete(s.versions, i, i+1)
			writeJSON(w, map[string]any{"fileId": id, "fileName": name})
			return
		}
	}
	writeError(w, http.StatusBadRequest, b2.CodeFileNotPresent, "File not present: "+name+" "+id)
}

func (s *Server) listUnfinished(w http.ResponseWriter, body map[string]any) {
	bucketID, prefix, start := str(body["bucketId"]), str(body["namePrefix"]), str(body["startFileId"])
	size := pageSize(body, 100, 100)
	files := []b2.File{}
	for _, v := range s.versions {
		if v.bucketID == bucketID && v.file.Action == actionStart && strings.HasPrefix(v.file.FileName, prefix) &&
			v.file.FileID >= start {
			files = append(files, v.file)
		}
	}
	var next any
	if len(files) > size {
		next = files[size].FileID
		files = files[:size]
	}
	writeJSON(w, map[string]any{fieldFiles: files, "nextFileId": next})
}

func (s *Server) cancelLargeFile(w http.ResponseWriter, body map[string]any) {
	id := str(body["fileId"])
	for i, v := range s.versions {
		if v.file.FileID == id && v.file.Action == actionStart {
			s.versions = slices.Delete(s.versions, i, i+1)
			writeJSON(w, map[string]any{"fileId": id, "fileName": v.file.FileName})
			return
		}
	}
	writeError(w, http.StatusBadRequest, b2.CodeBadRequest, "File not found: "+id)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "code": code, "message": message})
}
