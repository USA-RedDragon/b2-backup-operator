package tui

import (
	"context"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/types"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	listPaneWidth = 52
	twoPaneWidth  = 96
)

// tree is a snapshot's files by directory.
type tree struct {
	children map[string][]Node
	files    map[string]int
}

func buildTree(nodes []Node) *tree {
	t := &tree{children: map[string][]Node{}, files: map[string]int{}}
	for _, n := range nodes {
		dir := path.Dir(n.Path)
		t.children[dir] = append(t.children[dir], n)
		if n.Type == nodeDir {
			continue
		}
		for d := dir; ; d = path.Dir(d) {
			t.files[d]++
			if d == "/" {
				break
			}
		}
	}
	for dir := range t.children {
		slices.SortFunc(t.children[dir], func(a, b Node) int {
			if (a.Type == nodeDir) != (b.Type == nodeDir) {
				if a.Type == nodeDir {
					return -1
				}
				return 1
			}
			return strings.Compare(a.Path, b.Path)
		})
	}
	return t
}

// rootOf is the directory a snapshot's browsing starts in.
func rootOf(s *Snapshot) string {
	if len(s.Paths) == 1 {
		return s.Paths[0]
	}
	return "/"
}

// dumpFormat says which restore path a database dump takes.
func dumpFormat(p string) string {
	switch path.Ext(p) {
	case ".dmp":
		return "custom format"
	case ".sql":
		return "SQL dump"
	}
	return "plain SQL (old)"
}

// snapshotsScreen lists a backup's snapshots per source tag, with the files of
// the selected one beside them.
type snapshotsScreen struct {
	key      types.NamespacedName
	tags     []string
	dbTags   []string
	tag      int
	sel      int
	listTop  int
	browsing bool
	snapID   string
	dir      string
	treeSel  int
	treeTop  int
	trees    map[string]*tree
}

func newSnapshotsScreen(r *row) *snapshotsScreen {
	s := &snapshotsScreen{key: r.key(), trees: map[string]*tree{}}
	for i := range r.sb.Spec.Sources {
		src := &r.sb.Spec.Sources[i]
		tag := src.EffectiveTag()
		if slices.Contains(s.tags, tag) {
			continue
		}
		s.tags = append(s.tags, tag)
		if src.Type != b2backupv1.SourceTypeFiles {
			s.dbTags = append(s.dbTags, tag)
		}
	}
	return s
}

func (s *snapshotsScreen) typing(*Model) bool { return false }

func (s *snapshotsScreen) crumbs(*Model) []string {
	return []string{s.key.Namespace, s.key.Name, wordSnapshots}
}

func (s *snapshotsScreen) session(m *Model) (*session, sessionView) {
	ss := m.sessions.get(s.key)
	if ss == nil {
		return nil, sessionView{state: podFailed, err: errGone}
	}
	return ss, ss.view()
}

// allTags are the tags to page through: the sources', or, for a script
// backup, whatever its snapshots carry.
func (s *snapshotsScreen) allTags(v sessionView) []string {
	if len(s.tags) > 0 {
		return s.tags
	}
	var tags []string
	for _, snap := range v.snapshots {
		for _, t := range snap.Tags {
			if !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
	}
	slices.Sort(tags)
	return tags
}

func (s *snapshotsScreen) currentTag(v sessionView) string {
	tags := s.allTags(v)
	if len(tags) == 0 {
		return ""
	}
	s.tag = max(0, min(s.tag, len(tags)-1))
	return tags[s.tag]
}

func (s *snapshotsScreen) isDB(tag string) bool {
	return slices.Contains(s.dbTags, tag) || (len(s.tags) == 0 && tag != "files")
}

func (s *snapshotsScreen) list(v sessionView) []Snapshot {
	tag := s.currentTag(v)
	var out []Snapshot
	for _, snap := range v.snapshots {
		if tag == "" || slices.Contains(snap.Tags, tag) {
			out = append(out, snap)
		}
	}
	return out
}

func (s *snapshotsScreen) selected(v sessionView) *Snapshot {
	list := s.list(v)
	if len(list) == 0 {
		return nil
	}
	s.sel = max(0, min(s.sel, len(list)-1))
	return &list[s.sel]
}

func podStatus(m *Model, k types.NamespacedName, v sessionView) string {
	switch v.state {
	case podReady:
		return "pod " + v.podName + " " + styleOK.Render("ready")
	case podFailed:
		return styleFail.Render("pod failed")
	}
	text := "starting pod " + clock(m.now().Sub(v.started))
	if r := m.rowFor(k); r != nil {
		for i := range r.b2bkPods {
			p := &r.b2bkPods[i]
			if p.Labels[runner.LabelPurpose] != PodPurpose || p.DeletionTimestamp != nil {
				continue
			}
			phase := string(p.Status.Phase)
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
					phase = cs.State.Waiting.Reason
				}
			}
			text = "pod " + p.Name + " " + phase + " " + clock(m.now().Sub(v.started))
		}
	}
	return styleWarn.Render(text)
}

func (s *snapshotsScreen) headerRight(m *Model) string {
	_, v := s.session(m)
	parts := []string{podStatus(m, s.key, v)}
	tags := s.allTags(v)
	if len(tags) > 0 {
		cur := s.currentTag(v)
		var shown []string
		for _, t := range tags {
			if t == cur {
				shown = append(shown, styleBold.Render("["+t+"]"))
			} else {
				shown = append(shown, styleDim.Render(t))
			}
		}
		parts = append(parts, "tag "+strings.Join(shown, " "))
		if len(tags) > 1 {
			parts = append(parts, styleKey.Render(keyTab)+" switch tag")
		}
	}
	return strings.Join(parts, "   ")
}

func (s *snapshotsScreen) top(*Model, int) []string { return nil }

func snapTime(t time.Time, loc *time.Location, now time.Time) string {
	t = t.In(loc)
	if t.Year() != now.In(loc).Year() || now.Sub(t) > 180*24*time.Hour {
		return t.Format("2006-01-02")
	}
	return t.Format("01-02 15:04")
}

func (s *snapshotsScreen) body(m *Model, width, height int) []string {
	_, v := s.session(m)
	r := m.rowFor(s.key)
	if r == nil {
		return []string{"", styleDim.Render("  scheduledbackup/" + s.key.Name + " is gone.")}
	}
	switch {
	case v.state == podStarting:
		return s.starting(v, width)
	case v.state == podFailed:
		out := append([]string{""}, wrapped("the pod did not start: "+errText(v.err), width, "  ", styleFail)...)
		return append(out, "", styleDim.Render("  R tries again"))
	case !v.snapshotsDone:
		return []string{"", styleDim.Render("  listing snapshots with restic snapshots" + symEllipsis)}
	case v.snapshotsErr != nil:
		out := append([]string{""}, wrapped(errText(v.snapshotsErr), width, "  ", styleFail)...)
		return append(out, "", styleDim.Render("  R tries again"))
	}
	foot := listFooter(r, len(s.list(v)), width)
	height = max(1, height-1)
	if width < twoPaneWidth || s.isDB(s.currentTag(v)) {
		if s.browsing {
			return append(lines(s.treePane(m, v, width, height), height), foot)
		}
		return append(lines(s.listPane(m, r, v, width, height), height), foot)
	}
	left := s.listPane(m, r, v, listPaneWidth, height)
	right := s.treePane(m, v, width-listPaneWidth-3, height)
	out := make([]string, height, height+1)
	for i := range out {
		l, rt := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			rt = right[i]
		}
		out[i] = fit(l, listPaneWidth) + " " + styleDim.Render("│") + " " + rt
	}
	return append(out, foot)
}

func (s *snapshotsScreen) starting(v sessionView, width int) []string {
	out := make([]string, 0, 4+len(v.progress))
	out = append(out, "", "  Starting the restic pod b2bk snapshots uses. It takes a few seconds; the TUI keeps it",
		"  for this backup until you quit, so browsing is fast after this.", "")
	for _, line := range v.progress {
		out = append(out, styleDim.Render("  "+trunc(line, width-4)))
	}
	return out
}

func (s *snapshotsScreen) listPane(m *Model, r *row, v sessionView, width, height int) []string {
	list := s.list(v)
	now := m.now()
	loc := r.location()
	db := s.isDB(s.currentTag(v))
	out := []string{styleDim.Render(fmt.Sprintf("  %s %s %s %s", fit("ID", 9), fit("TIME", 12), fit("SIZE", 9),
		"PATHS"))}
	space := max(1, height-1)
	s.sel = max(0, min(s.sel, len(list)-1))
	s.listTop = clampOffset(s.listTop, s.sel, space, len(list))
	for i := s.listTop; i < min(len(list), s.listTop+space); i++ {
		snap := &list[i]
		size := symEm
		if snap.HasSize {
			size = humanBytes(snap.Size)
		}
		paths := strings.Join(snap.Paths, ", ")
		if db && len(snap.Paths) == 1 {
			paths += " " + styleDim.Render(dumpFormat(snap.Paths[0]))
		}
		cursor := " "
		if i == s.sel {
			cursor = styleAccent.Render(symCursor)
		}
		line := fmt.Sprintf("%s %s %s %s %s", cursor, fit(snap.ShortID, 9), fit(snapTime(snap.Time, loc, now), 12),
			fit(size, 9), paths)
		if i == s.sel && !s.browsing {
			line = styleSelected.Render(line)
		}
		out = append(out, trunc(line, width))
	}
	if len(list) == 0 {
		out = append(out, styleDim.Render("  no snapshots under this tag"))
	}
	return out
}

func listFooter(r *row, n, width int) string {
	foot := plural(n, "snapshot")
	if ret := retentionText(r.sb.Spec.Retention); r.sb.Spec.Retention != nil {
		foot += " · kept by " + strings.ReplaceAll(ret, " · ", ", ")
	}
	return styleDim.Render("  " + trunc(foot, width-2))
}

func (s *snapshotsScreen) treeOf(m *Model, snap *Snapshot) (*tree, string) {
	if t, ok := s.trees[snap.ID]; ok {
		return t, ""
	}
	ss := m.sessions.get(s.key)
	if ss == nil {
		return nil, ""
	}
	nodes, loaded, err := ss.filesOf(snap.ID)
	switch {
	case err != nil:
		return nil, errText(err)
	case !loaded:
		return nil, ""
	}
	t := buildTree(nodes)
	s.trees[snap.ID] = t
	return t, ""
}

func (s *snapshotsScreen) entries(m *Model, v sessionView) ([]Node, *Snapshot) {
	snap := s.selected(v)
	if snap == nil {
		return nil, nil
	}
	t, _ := s.treeOf(m, snap)
	if t == nil {
		return nil, snap
	}
	if s.snapID != snap.ID {
		s.snapID, s.dir, s.treeSel, s.treeTop = snap.ID, rootOf(snap), 0, 0
	}
	return t.children[s.dir], snap
}

func (s *snapshotsScreen) treePane(m *Model, v sessionView, width, height int) []string {
	snap := s.selected(v)
	if snap == nil {
		return nil
	}
	t, errMsg := s.treeOf(m, snap)
	switch {
	case errMsg != "":
		return []string{styleFail.Render(trunc(errMsg, width)), styleDim.Render("⏎ tries again")}
	case t == nil && !s.loadingFiles(m, snap):
		return []string{styleDim.Render("⏎ lists the files in " + snap.ShortID)}
	case t == nil:
		return []string{styleDim.Render("listing " + snap.ShortID + symEllipsis)}
	}
	entries, _ := s.entries(m, v)
	out := []string{styleBold.Render(s.dir) + styleDim.Render("  in "+snap.ShortID)}
	space := max(1, height-1)
	s.treeSel = max(0, min(s.treeSel, len(entries)-1))
	s.treeTop = clampOffset(s.treeTop, s.treeSel, space, len(entries))
	nameW := max(12, width-34)
	for i := s.treeTop; i < min(len(entries), s.treeTop+space); i++ {
		n := entries[i]
		name := path.Base(n.Path)
		var info string
		if n.Type == nodeDir {
			name += "/"
			info = fmt.Sprintf("%15s", plural(t.files[n.Path], "file"))
		} else {
			info = fmt.Sprintf("%9s  %s %d", humanBytes(n.Size), n.Mode.String(), n.UID)
		}
		cursor := " "
		if s.browsing && i == s.treeSel {
			cursor = styleAccent.Render(symCursor)
		}
		line := cursor + " " + fit(name, nameW) + " " + info
		if s.browsing && i == s.treeSel {
			line = styleSelected.Render(line)
		}
		out = append(out, trunc(line, width))
	}
	if len(entries) == 0 {
		out = append(out, styleDim.Render("  empty"))
	}
	return out
}

func (s *snapshotsScreen) loadingFiles(m *Model, snap *Snapshot) bool {
	ss := m.sessions.get(s.key)
	if ss == nil {
		return false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.filesLoaded[snap.ID]
}

// strip summarizes the latest snapshots under the other tags, with each
// database dump's format.
func (s *snapshotsScreen) strip(m *Model, width int) string {
	_, v := s.session(m)
	r := m.rowFor(s.key)
	if r == nil || !v.snapshotsDone {
		return ""
	}
	cur := s.currentTag(v)
	var parts []string
	for _, tag := range s.allTags(v) {
		if tag == cur {
			continue
		}
		var items []string
		for _, snap := range v.snapshots {
			if !slices.Contains(snap.Tags, tag) || len(items) == 2 {
				continue
			}
			item := snap.ShortID + " " + snapTime(snap.Time, r.location(), m.now()) + " " + strings.Join(snap.Paths, ", ")
			if s.isDB(tag) && len(snap.Paths) == 1 {
				item += " " + styleDim.Render(dumpFormat(snap.Paths[0]))
			}
			items = append(items, item)
		}
		if len(items) > 0 {
			parts = append(parts, styleBold.Render("tag "+tag+":")+"  "+strings.Join(items, "   ·   "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return trunc(" "+strings.Join(parts, "     "), width)
}

func (s *snapshotsScreen) bindings(*Model) []binding {
	if s.browsing {
		return []binding{
			{keys: []string{keyEnter}, disp: "⏎", label: "browse"},
			{keys: []string{keyRight}, disp: "→", label: "into folder"},
			{keys: []string{keyLeft}, disp: "←", label: "up a folder"},
			{disp: "r", label: "restore this path", act: actPod},
			{disp: "D", label: "download", act: actPod},
			{disp: "c", label: "compare with previous", act: actPod},
			{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
		}
	}
	return []binding{
		{keys: []string{keyEnter}, disp: "⏎", label: "browse"},
		{keys: []string{keyRight}, disp: "→", label: "into folder"},
		{disp: "r", label: "restore this snapshot", act: actPod},
		{disp: "D", label: "download file", act: actPod},
		{disp: "c", label: "compare with previous", act: actPod},
		{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
		{keys: []string{keyTab}, disp: keyTab, label: "switch tag", helpOnly: true},
		{disp: "R", label: "list the snapshots again, or start the pod again", helpOnly: true},
	}
}

func (s *snapshotsScreen) handleKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	ss, v := s.session(m)
	r := m.rowFor(s.key)
	k := msg.String()
	if k == "R" {
		switch {
		case v.state == podFailed && r != nil:
			m.sessions.open(s.key)
		case ss != nil && v.state == podReady:
			s.trees = map[string]*tree{}
			m.sessions.reloadSnapshots(ss)
		}
		return nil, true
	}
	if v.state != podReady || !v.snapshotsDone || r == nil {
		return nil, k != keyEsc
	}
	if s.browsing {
		return s.treeKey(m, r, v, k)
	}
	list := s.list(v)
	switch k {
	case keyUp, "k":
		s.sel = max(0, s.sel-1)
	case keyDown, "j":
		s.sel = min(len(list)-1, s.sel+1)
	case keyPgUp:
		s.sel = max(0, s.sel-10)
	case keyPgDown:
		s.sel = min(len(list)-1, s.sel+10)
	case keyTab:
		s.tag++
		if s.tag >= len(s.allTags(v)) {
			s.tag = 0
		}
		s.sel, s.listTop = 0, 0
	case keyEnter, keyRight:
		if snap := s.selected(v); snap != nil {
			m.sessions.loadFiles(ss, snap.ID)
			s.browsing = true
		}
	case "r":
		if snap := s.selected(v); snap != nil {
			return m.openRestore(r, snap, s.currentTag(v)), true
		}
	case "D":
		if snap := s.selected(v); snap != nil {
			return s.download(m, r, snap, rootOf(snap), len(snap.Paths) != 1 || !s.isDB(s.currentTag(v)),
				s.currentTag(v)), true
		}
	case "c":
		s.compare(m, v)
		return nil, true
	default:
		return nil, false
	}
	return nil, true
}

func (s *snapshotsScreen) treeKey(m *Model, r *row, v sessionView, k string) (tea.Cmd, bool) {
	entries, snap := s.entries(m, v)
	switch k {
	case keyUp, "k":
		s.treeSel = max(0, s.treeSel-1)
	case keyDown, "j":
		s.treeSel = min(len(entries)-1, s.treeSel+1)
	case keyEnter, keyRight, "l":
		if s.treeSel < len(entries) && entries[s.treeSel].Type == nodeDir {
			s.dir, s.treeSel, s.treeTop = entries[s.treeSel].Path, 0, 0
		} else if snap != nil {
			if ss := m.sessions.get(s.key); ss != nil {
				m.sessions.loadFiles(ss, snap.ID)
			}
		}
	case keyLeft, "backspace", "h":
		if snap != nil && s.dir != rootOf(snap) && s.dir != "/" {
			child := s.dir
			s.dir = path.Dir(s.dir)
			s.treeSel = 0
			if t := s.trees[snap.ID]; t != nil {
				for i, n := range t.children[s.dir] {
					if n.Path == child {
						s.treeSel = i
					}
				}
			}
		} else {
			s.browsing = false
		}
	case keyEsc:
		s.browsing = false
	case "r":
		if snap != nil {
			w := m.openRestore(r, snap, s.currentTag(v))
			if s.treeSel < len(entries) {
				if rs, ok := m.top().(*restoreScreen); ok {
					rs.setPath(entries[s.treeSel].Path)
				}
			}
			return w, true
		}
	case "D":
		if snap != nil && s.treeSel < len(entries) {
			return s.download(m, r, snap, entries[s.treeSel].Path, entries[s.treeSel].Type == nodeDir,
				s.currentTag(v)), true
		}
	case "c":
		s.compare(m, v)
		return nil, true
	default:
		return nil, false
	}
	return nil, true
}

func (s *snapshotsScreen) download(m *Model, r *row, snap *Snapshot, p string, folder bool, tag string) tea.Cmd {
	if !m.access.can(actPod) {
		return m.denied(actPod)
	}
	dir := "./" + r.sb.Namespace + "-" + snap.ShortID
	m.askText("Download "+p+" from "+snap.ShortID+" into", dir, func(dest string) tea.Cmd {
		if dest == "" {
			return nil
		}
		ss := m.sessions.get(s.key)
		if ss == nil {
			return nil
		}
		pod := ss.view().pod
		b := m.backend
		req := DownloadRequest{Namespace: s.key.Namespace, Backup: s.key.Name, Snapshot: snap.ID, Tag: tag, Path: p,
			Folder: folder, Dir: dest}
		st := m.run(func(ctx context.Context, w io.Writer) error { return b.Download(ctx, pod, req, w) })
		m.push(newOutputScreen(s.key, "download "+snap.ShortID, st))
		return nil
	})
	return nil
}

func (s *snapshotsScreen) compare(m *Model, v sessionView) {
	list := s.list(v)
	if s.sel+1 >= len(list) {
		m.setFlash("this is the oldest snapshot under the tag; there is nothing before it to compare with", styleDim)
		return
	}
	cur, prev := list[s.sel], list[s.sel+1]
	pod := v.pod
	b := m.backend
	st := m.run(func(ctx context.Context, w io.Writer) error { return b.Diff(ctx, pod, prev.ID, cur.ID, w) })
	m.push(newOutputScreen(s.key, "compare "+prev.ShortID+".."+cur.ShortID, st))
}

func (s *snapshotsScreen) click(m *Model, x, y int) tea.Cmd {
	_, v := s.session(m)
	if v.state != podReady || !v.snapshotsDone {
		return nil
	}
	twoPane := m.width >= twoPaneWidth
	if !twoPane && s.browsing || twoPane && x > listPaneWidth+1 {
		entries, _ := s.entries(m, v)
		i := s.treeTop + y - 1
		if y < 1 || i >= len(entries) {
			return nil
		}
		if s.browsing && i == s.treeSel && entries[i].Type == nodeDir {
			s.dir, s.treeSel, s.treeTop = entries[i].Path, 0, 0
			return nil
		}
		s.browsing, s.treeSel = true, i
		return nil
	}
	i := s.listTop + y - 1
	if y < 1 || i >= len(s.list(v)) {
		return nil
	}
	if i == s.sel && !s.browsing {
		if ss := m.sessions.get(s.key); ss != nil {
			m.sessions.loadFiles(ss, s.list(v)[i].ID)
			s.browsing = true
		}
		return nil
	}
	s.sel, s.browsing = i, false
	return nil
}

func (s *snapshotsScreen) scroll(m *Model, delta int) {
	_, v := s.session(m)
	if s.browsing {
		entries, _ := s.entries(m, v)
		s.treeSel = max(0, min(len(entries)-1, s.treeSel+delta))
		return
	}
	s.sel = max(0, min(len(s.list(v))-1, s.sel+delta))
}
