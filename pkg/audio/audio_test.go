package audio

import "testing"

func TestListDevices(t *testing.T) {
	devs, err := ListDevices()
	if err != nil {
		t.Skipf("skipping ListDevices in environment without audio hardware/server: %v", err)
	}
	if len(devs) == 0 {
		t.Log("no audio devices found")
		return
	}
	// Basic sanity checks on fields
	for _, d := range devs {
		if d.Name == "" {
			t.Errorf("device with empty name: %+v", d)
		}
		if d.DefaultSampleRate <= 0 {
			t.Errorf("invalid sample rate for device %s", d.Name)
		}
	}
}
