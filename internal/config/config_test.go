package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	c, err := parse(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.RetentionSnapshots != 168*time.Hour || c.IncidentPodThresh != 3 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestAllErrorsReportedAtOnce(t *testing.T) {
	env := map[string]string{"RETENTION_EVENTS": "30days", "STORAGE_BACKEND": "mysql", "LOG_LEVEL": "loud", "INCIDENT_WINDOW_MIN": "0"}
	_, err := parse(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"RETENTION_EVENTS", "STORAGE_BACKEND", "LOG_LEVEL", "INCIDENT_WINDOW_MIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %s: %v", want, err)
		}
	}
}
