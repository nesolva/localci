package web

import (
	"net/http"
	"strings"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/store"
)

type settingsView struct {
	Page
	Groups     []settingsGroup
	File       string
	FileFound  bool
	HasConfig  bool
	ConfigErr  string
	CheckSlots int
	JobSlots   int
	CheckInUse int
	JobInUse   int
	Machine    capacity.Machine
	MemGB      string
	DockerGB   string
}

type settingsGroup struct {
	Name  string
	Lines []settingLine
}

type settingLine struct {
	Key    string
	Value  string
	Source string
	Auto   bool
}

func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := settingsView{Page: Page{Title: "Settings", Nav: "settings", Live: true}, Machine: s.machine}
	v.Seq = s.seq(ctx)
	if s.config != nil {
		v.HasConfig = true
		eff, err := s.config()
		if err != nil {
			v.ConfigErr = err.Error()
		} else {
			v.File, v.FileFound = eff.File, eff.FileFound
			var g *settingsGroup
			for _, l := range eff.Lines() {
				name, _, _ := strings.Cut(l.Key, ".")
				if g == nil || g.Name != name {
					v.Groups = append(v.Groups, settingsGroup{Name: name})
					g = &v.Groups[len(v.Groups)-1]
				}
				g.Lines = append(g.Lines, settingLine{Key: l.Key, Value: l.Value, Source: string(l.Source), Auto: string(l.Source) == "auto"})
			}
			v.CheckSlots, v.JobSlots = eff.Capacity.CheckSlots, eff.Capacity.JobSlots
		}
	}
	if v.CheckSlots == 0 {
		v.CheckSlots, v.JobSlots = capacity.Slots(s.machine.CPUs, s.machine.FreeMemory, s.machine.DockerMemory)
	}
	// Use: a check holds one slot; a suite holds one per shard that is running.
	runs, err := s.st.Runs(ctx, 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, run := range runs {
		if run.State != store.StateRunning && run.State != store.StatePreparing {
			continue
		}
		if run.Kind == store.KindCheck {
			v.CheckInUse++
			continue
		}
		shards, err := s.st.Shards(ctx, run.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		n := 0
		for _, sh := range shards {
			if sh.State == store.StateRunning || sh.State == store.StatePreparing {
				n++
			}
		}
		v.JobInUse += max(n, 1)
	}
	v.MemGB, v.DockerGB = gib(s.machine.FreeMemory), gib(s.machine.DockerMemory)
	s.render(w, http.StatusOK, "settings", v)
}
