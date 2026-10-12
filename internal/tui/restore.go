package tui

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

// PodPurpose is the purpose label of the pod the TUI browses snapshots with.
const PodPurpose = "browse"

const (
	stepSnapshot = iota + 1
	stepTarget
	stepConfirm
	stepRunning
)

// restore wizard fields, in tab order.
const (
	focusTargets = iota
	focusClaim
	focusSize
	focusDir
	focusOnlyPath
	focusPath
	focusDelete
	focusDryRun
)

var targetOrder = []Target{TargetNewClaim, TargetDownload, TargetInPlace, TargetDatabase}

// restoreScreen walks through b2bk restore: snapshot, target, then a dry run
// and, for a target that overwrites, the name typed to confirm.
type restoreScreen struct {
	key  types.NamespacedName
	step int
	tag  string
	snap *Snapshot

	snapSel int
	target  Target
	focus   int

	claim, size, dir, path field
	onlyPath, del, dryRun  bool

	reasons      map[Target]string
	loaded       bool
	sourceClaim  string
	claimClass   string
	claimSize    string
	claimUsers   []string
	confirmField field
	dry          *stream
	run          *stream
	view         logView
	confirmErr   string
}

type targetsMsg struct {
	screen  *restoreScreen
	reasons map[Target]string
	claim   string
	class   string
	size    string
	users   []string
}

func (msg targetsMsg) route() {
	w := msg.screen
	w.reasons, w.loaded = msg.reasons, true
	w.sourceClaim, w.claimClass, w.claimSize, w.claimUsers = msg.claim, msg.class, msg.size, msg.users
	if w.size.value == "" {
		w.size.value = msg.size
	}
	if w.reasons[w.target] != "" {
		for _, t := range targetOrder {
			if w.reasons[t] == "" {
				w.target = t
				break
			}
		}
	}
}

func newRestoreScreen(r *row, snap *Snapshot, tag string) *restoreScreen {
	w := &restoreScreen{key: r.key(), step: stepSnapshot, tag: tag, dryRun: true}
	if w.tag == "" {
		if tags := backup.SourceTags(r.sb.Spec.Sources); len(tags) > 0 {
			w.tag = tags[0]
		}
	}
	if snap != nil {
		cp := *snap
		w.snap = &cp
		w.step = stepTarget
	}
	w.path.value = "/"
	w.setDefaults(r)
	return w
}

func (w *restoreScreen) setDefaults(r *row) {
	w.claim.value = r.sb.Namespace + "-restore"
	w.dir.value = "./" + r.sb.Namespace
	if w.snap != nil {
		loc := r.location()
		w.claim.value = r.sb.Namespace + "-restore-" + w.snap.Time.In(loc).Format("0102")
		w.dir.value = "./" + r.sb.Namespace + "-" + w.snap.ShortID
		if p := rootOf(w.snap); p != "/" {
			w.path.value = p
		}
	}
}

func (w *restoreScreen) setPath(p string) {
	w.path.value, w.onlyPath = p, true
}

// loadTargets asks b2bk which targets can be used, with why not, and reads
// the source claim the new claim copies.
func (w *restoreScreen) loadTargets(m *Model) tea.Cmd {
	w.loaded = false
	r := m.rowFor(w.key)
	if r == nil {
		return nil
	}
	b, ctx, k, tag := m.backend, m.ctx, w.key, w.tag
	sb := r.sb.DeepCopy()
	newClaimDenied := m.access.denied[actNewClaim]
	holdDenied := m.access.denied[actSuspend]
	return func() tea.Msg {
		reasons := b.RestoreTargets(ctx, k.Namespace, k.Name, tag)
		if reasons == nil {
			reasons = map[Target]string{}
		}
		if newClaimDenied != "" && reasons[TargetNewClaim] == "" {
			reasons[TargetNewClaim] = newClaimDenied
		}
		for _, t := range []Target{TargetInPlace, TargetDatabase} {
			if holdDenied != "" && reasons[t] == "" {
				reasons[t] = holdDenied + ", which holds the schedule while it writes"
			}
		}
		msg := targetsMsg{screen: w, reasons: reasons}
		msg.claim, msg.class, msg.size, msg.users = claimFacts(ctx, b, sb, tag)
		return msg
	}
}

// claimFacts reads the claim a files source is on: its class and size, and
// the pods that mount it.
func claimFacts(ctx context.Context, b Backend, sb *b2backupv1.ScheduledBackup, tag string) (
	claim, class, size string, users []string,
) {
	var vol *b2backupv1.VolumeSpec
	for i := range sb.Spec.Sources {
		src := &sb.Spec.Sources[i]
		if src.Type != b2backupv1.SourceTypeFiles || (tag != "" && src.EffectiveTag() != tag) {
			continue
		}
		for _, p := range src.ResolvedPaths(sb.EffectiveWorkingDir()) {
			if v := backup.MountFor(sb, p); v != nil {
				vol = v
				break
			}
		}
		if vol != nil {
			break
		}
	}
	if vol == nil {
		return "", "", "", nil
	}
	claim = vol.ExistingClaim
	rd, err := b.Reader()
	if err != nil {
		return claim, "", "", nil
	}
	var pvc corev1.PersistentVolumeClaim
	if err := rd.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: claim}, &pvc); err == nil {
		class = "the default class"
		if pvc.Spec.StorageClassName != nil {
			class = *pvc.Spec.StorageClassName
		}
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			size = q.String()
		}
	}
	var pods corev1.PodList
	if err := rd.List(ctx, &pods, client.InNamespace(sb.Namespace)); err == nil {
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Labels[labelManagedBy] == operatorManaged || p.Status.Phase == corev1.PodSucceeded ||
				p.Status.Phase == corev1.PodFailed {
				continue
			}
			for _, v := range p.Spec.Volumes {
				if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
					users = append(users, p.Name)
					break
				}
			}
		}
	}
	return claim, class, size, users
}

func (w *restoreScreen) typing(*Model) bool {
	switch w.step {
	case stepTarget:
		return slices.Contains([]int{focusClaim, focusSize, focusDir, focusPath}, w.focus)
	case stepConfirm:
		return overwrites(w.target)
	case stepRunning:
		return w.view.searching
	}
	return false
}

func overwrites(t Target) bool { return t == TargetInPlace || t == TargetDatabase }

func (w *restoreScreen) crumbs(*Model) []string {
	return []string{w.key.Namespace, w.key.Name, wordRestore}
}

func (w *restoreScreen) headerRight(m *Model) string {
	done := func(s string) string { return s + " " + styleOK.Render(symSucceeded) }
	switch w.step {
	case stepSnapshot:
		return "step 1 of 3 · " + styleBold.Render("snapshot") + " · target · confirm"
	case stepTarget:
		return "step 2 of 3 · " + done("snapshot") + " · " + styleBold.Render("target") + " · confirm"
	case stepConfirm:
		return "step 3 of 3 · " + done("snapshot") + " · " + done("target") + " · " + styleBold.Render("confirm")
	}
	return streamState(m, w.run.snapshot())
}

func (w *restoreScreen) top(*Model, int) []string { return nil }

func (w *restoreScreen) strip(*Model, int) string {
	if w.confirmErr != "" {
		return " " + styleFail.Render(w.confirmErr)
	}
	return ""
}

func (w *restoreScreen) snapLine(m *Model, r *row) string {
	if w.snap == nil {
		return ""
	}
	size := ""
	if w.snap.HasSize {
		size = "  " + humanBytes(w.snap.Size)
	}
	return fmt.Sprintf("  %s  %s  %s  %s %s%s", styleBold.Render("Snapshot"), w.snap.ShortID,
		snapTime(w.snap.Time, r.location(), m.now()), w.tag, strings.Join(w.snap.Paths, ", "), size)
}

func (w *restoreScreen) body(m *Model, width, height int) []string {
	r := m.rowFor(w.key)
	if r == nil {
		return []string{"", styleDim.Render("  scheduledbackup/" + w.key.Name + " is gone.")}
	}
	switch w.step {
	case stepSnapshot:
		return w.snapshotBody(m, r, width, height)
	case stepTarget:
		return w.targetBody(m, r, width)
	case stepConfirm:
		return w.confirmBody(m, r, width, height)
	}
	return w.view.render(w.run.snapshot(), r.location(), width, height)
}

func (w *restoreScreen) snapshots(m *Model) (sessionView, []Snapshot) {
	ss := m.sessions.get(w.key)
	if ss == nil {
		return sessionView{}, nil
	}
	v := ss.view()
	var out []Snapshot
	for _, s := range v.snapshots {
		if w.tag == "" || slices.Contains(s.Tags, w.tag) {
			out = append(out, s)
		}
	}
	return v, out
}

func (w *restoreScreen) snapshotBody(m *Model, r *row, width, height int) []string {
	v, list := w.snapshots(m)
	out := []string{"", "  " + styleBold.Render("Which snapshot?") + "  " + styleDim.Render("tag "+orDash(w.tag)), ""}
	switch {
	case v.state == podStarting:
		return append(out, styleWarn.Render("  "+podStatus(m, w.key, v)))
	case v.state == podFailed:
		return append(out, wrapped("the pod did not start: "+errText(v.err), width, "  ", styleFail)...)
	case !v.snapshotsDone:
		return append(out, styleDim.Render("  listing snapshots"+symEllipsis))
	case v.snapshotsErr != nil:
		return append(out, wrapped(errText(v.snapshotsErr), width, "  ", styleFail)...)
	case len(list) == 0:
		return append(out, styleDim.Render("  no snapshots under this tag"))
	}
	space := max(1, height-len(out))
	w.snapSel = max(0, min(w.snapSel, len(list)-1))
	first := clampOffset(0, w.snapSel, space, len(list))
	for i := first; i < min(len(list), first+space); i++ {
		s := &list[i]
		cursor := " "
		if i == w.snapSel {
			cursor = styleAccent.Render(symCursor)
		}
		size := symEm
		if s.HasSize {
			size = humanBytes(s.Size)
		}
		line := fmt.Sprintf("  %s %s %s %s %s", cursor, fit(s.ShortID, 9),
			fit(snapTime(s.Time, r.location(), m.now()), 11), fit(size, 9), strings.Join(s.Paths, ", "))
		if i == w.snapSel {
			line = styleSelected.Render(line)
		}
		out = append(out, trunc(line, width))
	}
	return out
}

func (w *restoreScreen) targetBody(m *Model, r *row, width int) []string {
	out := []string{w.snapLine(m, r), "", "  " + styleBold.Render("Where should it go?"), ""}
	if !w.loaded {
		return append(out, styleDim.Render("  checking which targets this backup can use"+symEllipsis))
	}
	for _, t := range targetOrder {
		out = append(out, w.targetLines(r, t, width)...)
	}
	out = append(out, "", w.optionsLine(width))
	return out
}

func (w *restoreScreen) targetLines(r *row, t Target, width int) []string {
	name, desc := w.describe(r, t)
	why := w.reasons[t]
	radio := symUnchecked
	if t == w.target {
		radio = symChecked
	}
	cursor := " "
	if t == w.target && w.focus == focusTargets {
		cursor = styleAccent.Render(symCursor)
	}
	if why != "" {
		reason := wrapped(sentence(why), width-34, "", styleDim)
		out := []string{styleDim.Render(fmt.Sprintf("    %s %s ", symUnchecked, fit(name, 27))) + reason[0]}
		if len(reason) > 1 {
			out = append(out, fit("", 34)+trunc(reason[1], width-34))
		}
		return out
	}
	if t == TargetDownload {
		w.dir.focused = w.focus == focusDir
		desc += "  folder " + w.dir.view(max(12, len(w.dir.value)+1))
	}
	line := fmt.Sprintf("  %s %s %s %s", cursor, radio, fit(name, 27), desc)
	if t == w.target {
		line = styleSelected.Render(fmt.Sprintf("  %s %s %s ", cursor, radio, fit(name, 27))) + desc
	}
	out := []string{trunc(line, width)}
	if t != w.target {
		return out
	}
	if t == TargetNewClaim {
		class := w.claimClass + " (from " + w.sourceClaim + ")"
		if w.claimClass == "" {
			class = "as pvc/" + w.sourceClaim
		}
		w.size.focused, w.claim.focused = w.focus == focusSize, w.focus == focusClaim
		out = append(out, trunc(fmt.Sprintf("        name  %s   size  %s   class  %s",
			w.claim.view(max(12, len(w.claim.value)+1)), w.size.view(max(5, len(w.size.value)+1)), class), width))
	}
	return out
}

func (w *restoreScreen) describe(r *row, t Target) (string, string) {
	switch t {
	case TargetNewClaim:
		return "A new claim", "Restore into a new PVC next to the app. Nothing live is touched."
	case TargetDownload:
		return "Download to this computer", "Stream files into a local folder."
	case TargetInPlace:
		where := "the source"
		if w.snap != nil {
			where = strings.Join(w.snap.Paths, ", ")
		}
		return "Over the live volume", "Replace " + where + " in place. Suspends the schedule while it runs."
	}
	name := "the database"
	if db := r.sb.Spec.Database; db != nil {
		name = "the " + string(db.Engine) + " database"
	}
	return "Into the database", "Stream the dump back into " + name + ". Holds the schedule while it runs."
}

func checkbox(on, focused bool, label string) string {
	box := "[ ]"
	if on {
		box = "[x]"
	}
	if focused {
		box = styleAccent.Render(box)
		label = styleSelected.Render(label)
	}
	return box + " " + label
}

func (w *restoreScreen) optionsLine(width int) string {
	w.path.focused = w.focus == focusPath
	parts := []string{
		checkbox(w.onlyPath, w.focus == focusOnlyPath, "only path") + "  " + w.path.view(max(12, len(w.path.value)+1)),
	}
	if w.target != TargetDownload && w.target != TargetDatabase {
		parts = append(parts, checkbox(w.del, w.focus == focusDelete, "remove files not in the snapshot"))
	}
	parts = append(parts, checkbox(w.dryRun, w.focus == focusDryRun, "dry run first"))
	return trunc("  "+styleBold.Render("Options")+"   "+strings.Join(parts, "   "), width)
}

// fields are the focusable fields of the target step, in tab order.
func (w *restoreScreen) fields() []int {
	f := []int{focusTargets}
	switch w.target {
	case TargetNewClaim:
		f = append(f, focusClaim, focusSize)
	case TargetDownload:
		f = append(f, focusDir)
	}
	f = append(f, focusOnlyPath, focusPath)
	if w.target != TargetDownload && w.target != TargetDatabase {
		f = append(f, focusDelete)
	}
	return append(f, focusDryRun)
}

func (w *restoreScreen) request() RestoreRequest {
	req := RestoreRequest{
		Namespace: w.key.Namespace, Backup: w.key.Name, Tag: w.tag, Target: w.target,
		Claim: w.claim.value, Size: w.size.value, Dir: w.dir.value, Delete: w.del && w.target != TargetDownload,
	}
	if w.snap != nil {
		req.Snapshot = w.snap.ID
	}
	if w.onlyPath && w.path.value != "" && w.path.value != "/" {
		req.Path = w.path.value
	}
	return req
}

func (w *restoreScreen) confirmBody(m *Model, r *row, width, height int) []string {
	var out []string
	label := func(name, value string) {
		out = append(out, "  "+fit(styleBold.Render(name), 15)+value)
	}
	req := w.request()
	switch w.target {
	case TargetInPlace:
		out = append(out, "  "+styleWarn.Render("This overwrites the live volume "+orDash(w.sourceClaim)+" in "+w.key.Namespace+"."))
	case TargetDatabase:
		out = append(out, "  "+styleWarn.Render("This overwrites the live database of "+w.key.Namespace+"."))
	case TargetNewClaim:
		out = append(out, "  This restores into a new claim, "+req.Claim+". Nothing live is touched.")
	case TargetDownload:
		out = append(out, "  This downloads into "+req.Dir+" on this computer. Nothing in the cluster changes.")
	}
	out = append(out, "")
	snap := symEm
	if w.snap != nil {
		snap = w.snap.ShortID + "  " + snapTime(w.snap.Time, r.location(), m.now()) + "  " + strings.Join(w.snap.Paths, ", ")
	}
	if req.Path != "" {
		snap += styleDim.Render("  only " + req.Path)
	}
	label("Snapshot", snap)
	label("Target", w.targetText(req))
	if w.dryRun {
		out = append(out, w.dryRunLines(m, width, height-len(out)-8)...)
	}
	if overwrites(w.target) {
		out = append(out, "")
		label("While it runs", "the "+w.key.Name+" schedule is held, then released if this restore succeeds.")
		if w.target == TargetInPlace {
			label("", styleDim.Render("Scale the app down first if it writes during the restore."))
		}
		out = append(out, "")
		w.confirmField.focused = true
		out = append(out, fmt.Sprintf("  Type %s to confirm:  %s", styleBold.Render(w.key.Name), w.confirmField.view(20)))
	}
	return out
}

func (w *restoreScreen) targetText(req RestoreRequest) string {
	switch w.target {
	case TargetInPlace:
		text := "claim " + orDash(w.sourceClaim)
		if w.snap != nil {
			text += " at " + strings.Join(w.snap.Paths, ", ")
		}
		if len(w.claimUsers) > 0 {
			text += styleDim.Render("  (pod " + strings.Join(w.claimUsers, ", ") + " is using it)")
		}
		return text
	case TargetDatabase:
		return "the database of scheduledbackup/" + w.key.Name
	case TargetNewClaim:
		size := req.Size
		if size == "" {
			size = orDash(w.claimSize)
		}
		return "new claim " + req.Claim + "  " + styleDim.Render(size+", laid out like "+orDash(w.sourceClaim))
	}
	return req.Dir
}

func (w *restoreScreen) dryRunLines(m *Model, width, space int) []string {
	if w.dry == nil {
		return nil
	}
	sv := w.dry.snapshot()
	pad := "  " + fit("", 15)
	head := "  " + fit(styleBold.Render("Dry run"), 15)
	if !sv.done {
		out := []string{head + styleWarn.Render("running "+clock(m.now().Sub(sv.started))+symEllipsis)}
		for _, l := range tail(sv.lines, 3) {
			out = append(out, pad+styleDim.Render(trunc(l.text, width-20)))
		}
		return out
	}
	if sv.err != nil {
		msg := wrapped("failed: "+errText(sv.err), width-17, "", styleFail)
		out := []string{head + msg[0]}
		for _, line := range msg[1:] {
			out = append(out, pad+line)
		}
		for _, l := range tail(sv.lines, 3) {
			out = append(out, pad+styleDim.Render(trunc(l.text, width-20)))
		}
		return out
	}
	sum := summarizeDryRun(sv.lines, w.target)
	out := []string{head + sum.text}
	for _, item := range sum.items[:min(len(sum.items), max(0, space))] {
		out = append(out, pad+"  "+trunc(item, width-22))
	}
	return out
}

func tail(l []logLine, n int) []logLine {
	return l[max(0, len(l)-n):]
}

type dryRunSummary struct {
	text  string
	items []string
}

// summarizeDryRun counts what restic restore --dry-run --verbose=2 reports.
// A download or a new claim is planned with restic ls, which lists entries.
func summarizeDryRun(l []logLine, t Target) dryRunSummary {
	var sum dryRunSummary
	switch t {
	case TargetDatabase:
		for _, line := range tail(l, 4) {
			sum.items = append(sum.items, line.text)
		}
		sum.text = "restoredb accepted the dump"
		return sum
	case TargetDownload, TargetNewClaim:
		n := 0
		for _, line := range l {
			if f := strings.Fields(line.text); len(f) >= 6 && len(f[0]) == 10 && strings.ContainsAny(f[0][:1], "-dlLs") {
				n++
				if len(sum.items) < 6 && f[0][0] == '-' {
					sum.items = append(sum.items, f[len(f)-1])
				}
			}
		}
		sum.text = commas(n) + " files and folders would be restored"
		return sum
	}
	counts := map[string]int{}
	for _, line := range l {
		verb, rest, ok := strings.Cut(line.text, " ")
		if !ok {
			continue
		}
		switch verb {
		case "restored", "updated", "unchanged":
			if !strings.Contains(rest, " with size ") {
				continue
			}
			fallthrough
		case "deleted":
			counts[verb]++
			if verb != "unchanged" && len(sum.items) < 6 {
				sum.items = append(sum.items, dryRunItem(verb, strings.TrimSpace(rest)))
			}
		}
	}
	sum.text = fmt.Sprintf("%s would change, %s removed, %s unchanged",
		plural(counts["restored"]+counts["updated"], "file"), commas(counts["deleted"]), commas(counts["unchanged"]))
	return sum
}

func (w *restoreScreen) bindings(*Model) []binding {
	switch w.step {
	case stepSnapshot:
		return []binding{
			{keys: []string{keyUp}, disp: "↑↓", label: "choose"},
			{keys: []string{keyTab}, disp: keyTab, label: "switch tag"},
			{keys: []string{keyEnter}, disp: "⏎", label: "continue"},
			{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
		}
	case stepTarget:
		return []binding{
			{keys: []string{keyUp}, disp: "↑↓", label: "choose"},
			{keys: []string{keySpace}, disp: keySpace, label: "toggle"},
			{keys: []string{keyTab}, disp: keyTab, label: "next field"},
			{keys: []string{keyEnter}, disp: "⏎", label: "continue"},
			{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
		}
	case stepConfirm:
		return []binding{
			{keys: []string{keyEnter}, disp: "⏎", label: wordRestore},
			{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
		}
	}
	return []binding{
		{disp: "f", label: wordFollow},
		{disp: "w", label: wordWrap},
		{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
	}
}

func (w *restoreScreen) handleKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	r := m.rowFor(w.key)
	if r == nil {
		return nil, false
	}
	w.confirmErr = ""
	switch w.step {
	case stepSnapshot:
		return w.snapshotKey(m, r, msg)
	case stepTarget:
		return w.targetKey(m, msg)
	case stepConfirm:
		return w.confirmKey(m, msg)
	}
	if w.view.key(msg) {
		return nil, true
	}
	return nil, false
}

func (w *restoreScreen) snapshotKey(m *Model, r *row, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	_, list := w.snapshots(m)
	switch msg.String() {
	case keyUp, "k":
		w.snapSel = max(0, w.snapSel-1)
	case keyDown, "j":
		w.snapSel = min(len(list)-1, w.snapSel+1)
	case keyTab:
		tags := backup.SourceTags(r.sb.Spec.Sources)
		if i := slices.Index(tags, w.tag); len(tags) > 1 {
			w.tag = tags[(i+1)%len(tags)]
			w.snapSel = 0
			return w.loadTargets(m), true
		}
	case keyEnter:
		if w.snapSel < len(list) {
			snap := list[w.snapSel]
			w.snap = &snap
			w.setDefaults(r)
			w.step = stepTarget
		}
	default:
		return nil, false
	}
	return nil, true
}

func (w *restoreScreen) targetKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	k := msg.String()
	fields := w.fields()
	at := max(0, slices.Index(fields, w.focus))
	switch k {
	case keyTab:
		w.focus = fields[(at+1)%len(fields)]
		return nil, true
	case "shift+tab":
		w.focus = fields[(at+len(fields)-1)%len(fields)]
		return nil, true
	case keyEsc:
		if w.focus != focusTargets {
			w.focus = focusTargets
			return nil, true
		}
		return nil, false
	case keyEnter:
		w.toConfirm(m)
		return nil, true
	}
	switch w.focus {
	case focusTargets:
		w.chooseTarget(k)
		return nil, true
	case focusClaim:
		return nil, w.claim.edit(msg)
	case focusSize:
		return nil, w.size.edit(msg)
	case focusDir:
		return nil, w.dir.edit(msg)
	case focusPath:
		if w.path.edit(msg) {
			w.onlyPath = w.path.value != "" && w.path.value != "/"
			return nil, true
		}
		return nil, false
	}
	if k == keySpace || k == "x" {
		switch w.focus {
		case focusOnlyPath:
			w.onlyPath = !w.onlyPath
		case focusDelete:
			w.del = !w.del
		case focusDryRun:
			w.dryRun = !w.dryRun
		}
		return nil, true
	}
	if k == keyUp || k == keyDown {
		w.focus = focusTargets
		w.chooseTarget(k)
		return nil, true
	}
	return nil, false
}

func (w *restoreScreen) chooseTarget(k string) {
	i := slices.Index(targetOrder, w.target)
	step := 0
	switch k {
	case keyUp, "k":
		step = -1
	case keyDown, "j":
		step = 1
	default:
		return
	}
	for j := i + step; j >= 0 && j < len(targetOrder); j += step {
		if w.reasons[targetOrder[j]] == "" {
			w.target = targetOrder[j]
			return
		}
	}
}

func (w *restoreScreen) toConfirm(m *Model) {
	if !w.loaded || w.reasons[w.target] != "" {
		w.confirmErr = "choose a target this backup can use"
		return
	}
	if w.target == TargetNewClaim && w.claim.value == "" {
		w.confirmErr = "name the new claim"
		return
	}
	if w.target == TargetDownload && w.dir.value == "" {
		w.confirmErr = "name the folder to download into"
		return
	}
	w.step = stepConfirm
	w.confirmField.value = ""
	w.dry = nil
	if !w.dryRun {
		return
	}
	req := w.request()
	req.DryRun = true
	b := m.backend
	w.dry = m.run(func(ctx context.Context, out io.Writer) error { return b.Restore(ctx, req, out) })
}

func (w *restoreScreen) confirmKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case keyEsc:
		if w.dry != nil {
			w.dry.stop()
		}
		w.step = stepTarget
		return nil, true
	case keyEnter:
		w.start(m)
		return nil, true
	}
	if overwrites(w.target) {
		return nil, w.confirmField.edit(msg)
	}
	return nil, false
}

func (w *restoreScreen) start(m *Model) {
	if w.dry != nil {
		sv := w.dry.snapshot()
		switch {
		case !sv.done:
			w.confirmErr = "wait for the dry run to finish"
			return
		case sv.err != nil:
			w.confirmErr = "the dry run failed, so the restore would too; esc goes back"
			return
		}
	}
	req := w.request()
	if overwrites(w.target) {
		if w.confirmField.value != w.key.Name {
			w.confirmErr = "type " + w.key.Name + " exactly to confirm"
			return
		}
		req.Confirm = w.confirmField.value
	}
	b := m.backend
	w.run = m.run(func(ctx context.Context, out io.Writer) error { return b.Restore(ctx, req, out) })
	w.step = stepRunning
	w.view = logView{follow: true}
}

// running reports whether the restore itself is writing.
func (w *restoreScreen) running() bool {
	return w.step == stepRunning && !w.run.snapshot().done
}

func (w *restoreScreen) closeScreen() {
	if w.dry != nil {
		w.dry.stop()
	}
}

func (w *restoreScreen) click(m *Model, _, y int) tea.Cmd {
	if w.step != stepTarget || !w.loaded {
		return nil
	}
	r := m.rowFor(w.key)
	if r == nil {
		return nil
	}
	line := 4
	for _, t := range targetOrder {
		n := len(w.targetLines(r, t, m.width))
		if y >= line && y < line+n && w.reasons[t] == "" {
			w.target, w.focus = t, focusTargets
			return nil
		}
		line += n
	}
	return nil
}

func (w *restoreScreen) scroll(_ *Model, delta int) {
	if w.step == stepRunning {
		w.view.scroll(delta)
	}
}

// dryRunItem renders a restic restore --verbose=2 line as path, size and change.
func dryRunItem(verb, rest string) string {
	p, size, _ := strings.Cut(rest, " with size ")
	change := map[string]string{"restored": "written", "updated": "modified", "deleted": "removed"}[verb]
	return fit(p, 36) + " " + fmt.Sprintf("%9s", size) + "  " + change
}

// sentence starts an error message with a capital, as a line of its own.
func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
