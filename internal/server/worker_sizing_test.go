package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestValidateWorkerSizing(t *testing.T) {
	cases := []struct {
		name    string
		form    url.Values
		wantErr string
	}{
		{"all blank means same as control plane", url.Values{"worker_num_cores": {""}, "worker_memory_mib": {"  "}}, ""},
		{"valid override", url.Values{"worker_num_cores": {"8"}, "worker_memory_mib": {"16384"}, "worker_boot_volume_size": {"200"}}, ""},
		{"non-numeric is rejected, not silently ignored", url.Values{"worker_num_cores": {"lots"}}, "worker CPU cores"},
		{"zero cores", url.Values{"worker_num_cores": {"0"}}, "worker CPU cores"},
		{"too little memory", url.Values{"worker_memory_mib": {"128"}}, "worker memory"},
		{"too small disk", url.Values{"worker_boot_volume_size": {"5"}}, "worker boot disk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/clusters/preview", strings.NewReader(tc.form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			err := validateWorkerSizing(r)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}
