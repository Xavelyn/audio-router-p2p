//go:build !linux

package discovery

// DiscoverDevices returns empty results on non-Linux platforms (macOS CoreAudio, Windows WASAPI).
func DiscoverDevices() ([]DiscoveredDevice, error) {
	return nil, nil
}
