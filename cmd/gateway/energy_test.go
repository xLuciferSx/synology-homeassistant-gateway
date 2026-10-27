package main

import (
	"fmt"
	"testing"

	"hearth/internal/model"
)

func TestEnergyLedger(t *testing.T) {
	dir := t.TempDir()
	e := loadEnergy(dir)
	for i := 1; i <= 65; i++ {
		e.record(model.Device{Kind: model.KindEnergy, State: map[string]any{
			"elec_day": fmt.Sprintf("2026-%02d-%02d", 8+i/31, 1+i%28), "elec_yesterday_kwh": 10.0, "elec_yesterday_cost": 2.0,
		}})
	}
	e.record(model.Device{Kind: model.KindEnergy, State: map[string]any{
		"elec_day": "2026-10-24", "elec_yesterday_kwh": 12.5, "elec_peak_kwh": 4.0, "elec_offpeak_kwh": 8.5,
		"gas_day": "2026-10-24", "gas_yesterday_kwh": 30.0, "gas_yesterday_cost": 1.8,
	}})
	days := loadEnergy(dir).list()
	if len(days) > energyKeep {
		t.Fatalf("kept %d days", len(days))
	}
	last := days[len(days)-1]
	if last.Day != "2026-10-24" || last.ElecKWh != 12.5 || last.GasCost != 1.8 || last.ElecOffPeakKWh != 8.5 {
		t.Fatalf("got %+v", last)
	}
}
