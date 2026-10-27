package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"hearth/internal/model"
)

// EnergyDay is one day's metered use, as Octopus reports it a day or two later.
type EnergyDay struct {
	Day            string  `json:"day"`
	ElecKWh        float64 `json:"elec_kwh"`
	ElecCost       float64 `json:"elec_cost"`
	ElecPeakKWh    float64 `json:"elec_peak_kwh"`
	ElecOffPeakKWh float64 `json:"elec_offpeak_kwh"`
	GasKWh         float64 `json:"gas_kwh"`
	GasCost        float64 `json:"gas_cost"`
}

const energyKeep = 60

// energyLedger keeps the last couple of months of daily totals.
type energyLedger struct {
	mu   sync.Mutex
	path string
	days map[string]EnergyDay
	heat *heatingTime
}

func loadEnergy(dir string) *energyLedger {
	e := &energyLedger{path: filepath.Join(dir, "energy.json"), days: map[string]EnergyDay{}}
	if b, err := os.ReadFile(e.path); err == nil {
		_ = json.Unmarshal(b, &e.days)
	}
	return e
}

func (e *energyLedger) record(d model.Device) {
	if d.Kind != model.KindEnergy {
		return
	}
	f := func(k string) float64 { v, _ := d.State[k].(float64); return v }
	s := func(k string) string { v, _ := d.State[k].(string); return v }
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	if day := s("elec_day"); day != "" {
		x := e.days[day]
		x.Day, x.ElecKWh, x.ElecCost = day, f("elec_yesterday_kwh"), f("elec_yesterday_cost")
		x.ElecPeakKWh, x.ElecOffPeakKWh = f("elec_peak_kwh"), f("elec_offpeak_kwh")
		changed = changed || x != e.days[day]
		e.days[day] = x
	}
	if day := s("gas_day"); day != "" {
		x := e.days[day]
		x.Day, x.GasKWh, x.GasCost = day, f("gas_yesterday_kwh"), f("gas_yesterday_cost")
		changed = changed || x != e.days[day]
		e.days[day] = x
	}
	if !changed {
		return
	}
	list := e.sortedLocked()
	for len(list) > energyKeep {
		delete(e.days, list[0].Day)
		list = list[1:]
	}
	if b, err := json.Marshal(e.days); err == nil {
		_ = os.WriteFile(e.path, b, 0o600)
	}
}

func (e *energyLedger) sortedLocked() []EnergyDay {
	out := make([]EnergyDay, 0, len(e.days))
	for _, d := range e.days {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

func (e *energyLedger) list() []EnergyDay {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sortedLocked()
}

func (e *energyLedger) routes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/energy/days", auth(func(w http.ResponseWriter, _ *http.Request) {
		out := map[string]any{"days": e.list()}
		if e.heat != nil {
			out["heating"] = e.heat.minutes()
		}
		writeJSON(w, http.StatusOK, out)
	}))
}
