package main

import "testing"

func TestCategoryOf(t *testing.T) {
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{Icon: "exclamationmark.triangle.fill", Level: "alert", Text: "Water leak detected by Sink sensor"}, catSafety},
		{Entry{Icon: "exclamationmark.triangle.fill", Level: "alert", Text: "Hoover needs help"}, catJobs},
		{Entry{Icon: "shield.lefthalf.filled", Level: "alert", Text: "Alarm triggered"}, catSafety},
		{Entry{Icon: "shield.lefthalf.filled", Level: "info", Text: "Alarm armed away"}, ""},
		{Entry{Icon: "washer.fill", Level: "alert", Text: "Washing Machine has finished"}, catJobs},
		{Entry{Icon: "dishwasher.fill", Level: "alert", Text: "The dishes are clean"}, catJobs},
		{Entry{Icon: "bolt.car.fill", Level: "info", Text: "Car started charging"}, ""},
		{Entry{Icon: "house.fill", Text: "Alex arrived home"}, catPeople},
		{Entry{Icon: "wifi.slash", Level: "warning", Text: "TV went offline"}, catDevices},
		{Entry{Icon: "lock.fill", Text: "Front door locked"}, ""},
	}
	for _, c := range cases {
		if got := categoryOf(c.e); got != c.want {
			t.Errorf("%q: got %q, want %q", c.e.Text, got, c.want)
		}
	}
}
