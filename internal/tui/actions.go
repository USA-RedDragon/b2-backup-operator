package tui

import (
	"context"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The actions a backup offers from the dashboard and its detail screen.

func (m *Model) backupNow(r *row) tea.Cmd {
	if !m.access.can(actBackup) {
		return m.denied(actBackup)
	}
	key := r.key()
	m.ask(fmt.Sprintf("Back up %s now?  y yes  any other key no", r.title()), func(k string) tea.Cmd {
		if k != "y" {
			return nil
		}
		m.triggered[key] = m.now()
		b := m.backend
		return m.do(func(ctx context.Context) (string, error) {
			out, err := b.BackupNow(ctx, key.Namespace, key.Name)
			return summary(out), err
		})
	})
	return nil
}

func (m *Model) togglePause(r *row) tea.Cmd {
	if !m.access.can(actSuspend) {
		return m.denied(actSuspend)
	}
	key, suspend := r.key(), !r.sb.Spec.Suspend
	b := m.backend
	return m.do(func(ctx context.Context) (string, error) {
		out, err := b.SetSuspend(ctx, key.Namespace, key.Name, suspend)
		return summary(out), err
	})
}

func (m *Model) cancelRun(r *row) tea.Cmd {
	if !m.access.can(actCancel) {
		return m.denied(actCancel)
	}
	job := r.running()
	if job == nil {
		m.setFlash("no backup of "+r.title()+" is running", styleDim)
		return nil
	}
	key := r.key()
	m.ask(fmt.Sprintf("Cancel job/%s of %s? restic may leave a lock behind.  y yes  any other key no",
		job.Name, r.title()), func(k string) tea.Cmd {
		if k != "y" {
			return nil
		}
		b := m.backend
		return m.do(func(ctx context.Context) (string, error) {
			out, err := b.Cancel(ctx, key.Namespace, key.Name)
			return summary(out), err
		})
	})
	return nil
}

func (m *Model) checkPrompt(r *row) tea.Cmd {
	if !m.access.can(actPod) {
		return m.denied(actPod)
	}
	key := r.key()
	repo := r.sb.Spec.RepositoryRef.Name
	m.askText(fmt.Sprintf("Check repository/%s; also read this share of the data (blank: metadata only, e.g. 5%% or 1/10)",
		repo), "", func(readData string) tea.Cmd {
		b := m.backend
		s := m.run(func(ctx context.Context, w io.Writer) error {
			return b.Check(ctx, key.Namespace, "sb/"+key.Name, readData, w)
		})
		title := "check"
		if readData != "" {
			title += " --read-data-subset=" + readData
		}
		m.push(newOutputScreen(key, title, s))
		return nil
	})
	return nil
}

func (m *Model) openDetail(r *row) tea.Cmd {
	m.push(newDetail(r.key()))
	return m.fetchLastLines(r)
}

func (m *Model) openLogs(r *row, job string) tea.Cmd {
	if !m.access.can(actLogs) {
		return m.denied(actLogs)
	}
	if job == "" {
		if running := r.running(); running != nil {
			job = running.Name
		} else if len(r.jobs) > 0 {
			job = r.jobs[0].Name
		} else if h := r.sb.Status.History; len(h) > 0 {
			job = h[0].JobName
		}
	}
	if job == "" {
		m.setFlash("no run of "+r.title()+" is left in the cluster to read logs from", styleDim)
		return nil
	}
	m.push(newLogsScreen(m, r.key(), job))
	return nil
}

func (m *Model) openSnapshots(r *row) tea.Cmd {
	if !m.access.can(actPod) {
		return m.denied(actPod)
	}
	m.sessions.open(r.key())
	m.push(newSnapshotsScreen(r))
	return nil
}

func (m *Model) openDoctor(r *row) tea.Cmd {
	d := newDoctorScreen(r.key())
	m.push(d)
	return d.run(m)
}

func (m *Model) openRestore(r *row, snap *Snapshot, tag string) tea.Cmd {
	if !m.access.can(actPod) {
		return m.denied(actPod)
	}
	m.sessions.open(r.key())
	w := newRestoreScreen(r, snap, tag)
	m.push(w)
	return w.loadTargets(m)
}

func (m *Model) denied(act action) tea.Cmd {
	m.setFlash(m.access.denied[act], styleFail)
	return nil
}

// fetchLastLines reads the last log line of each failed run still present.
func (m *Model) fetchLastLines(r *row) tea.Cmd {
	if !m.access.can(actLogs) {
		return nil
	}
	var cmds []tea.Cmd
	b, ctx, ns := m.backend, m.ctx, r.sb.Namespace
	for i := range r.jobs {
		job := r.jobs[i].Name
		if r.jobs[i].Status.Failed == 0 {
			continue
		}
		if _, ok := m.lastLines[job]; ok {
			continue
		}
		m.lastLines[job] = ""
		cmds = append(cmds, func() tea.Msg {
			return lastLineMsg{job: job, line: b.LastLogLine(ctx, ns, job)}
		})
	}
	return tea.Batch(cmds...)
}

// summary joins what a command printed into one line.
func summary(out string) string {
	var parts []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}
