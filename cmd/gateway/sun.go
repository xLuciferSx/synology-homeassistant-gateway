package main

import (
	"math"
	"time"
)

// sunTimes returns local sunrise and sunset for the given day and place, using
// the NOAA approximation (accurate to about a minute). ok is false in polar
// day/night, when the sun doesn't cross the horizon.
func sunTimes(day time.Time, lat, lon float64) (rise, set time.Time, ok bool) {
	loc := day.Location()
	y, m, d := day.Date()
	noonUTC := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	n := float64(noonUTC.YearDay())

	// fractional year (radians)
	g := 2 * math.Pi / 365 * (n - 1)
	eqTime := 229.18 * (0.000075 + 0.001868*math.Cos(g) - 0.032077*math.Sin(g) -
		0.014615*math.Cos(2*g) - 0.040849*math.Sin(2*g)) // minutes
	decl := 0.006918 - 0.399912*math.Cos(g) + 0.070257*math.Sin(g) - 0.006758*math.Cos(2*g) +
		0.000907*math.Sin(2*g) - 0.002697*math.Cos(3*g) + 0.00148*math.Sin(3*g)

	latR := lat * math.Pi / 180
	zenith := 90.833 * math.Pi / 180 // includes refraction and the sun's radius
	cosH := math.Cos(zenith)/(math.Cos(latR)*math.Cos(decl)) - math.Tan(latR)*math.Tan(decl)
	if cosH < -1 || cosH > 1 {
		return time.Time{}, time.Time{}, false
	}
	ha := math.Acos(cosH) * 180 / math.Pi // degrees

	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	at := func(minutes float64) time.Time {
		return midnight.Add(time.Duration(minutes * float64(time.Minute))).In(loc)
	}
	rise = at(720 - 4*(lon+ha) - eqTime)
	set = at(720 - 4*(lon-ha) - eqTime)
	return rise, set, true
}
