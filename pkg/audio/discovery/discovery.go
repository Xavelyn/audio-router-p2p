package discovery

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/audio-router-p2p/pkg/audio/urn"
)

// DiscoveredDevice represents an audio endpoint discovered directly from kernel virtual filesystems.
type DiscoveredDevice struct {
	CardIndex   int      `json:"card_index"`
	DeviceIndex int      `json:"device_index"`
	CardID      string   `json:"card_id"`
	CardName    string   `json:"card_name"`
	VendorID    string   `json:"vendor_id,omitempty"`
	ProductID   string   `json:"product_id,omitempty"`
	Serial      string   `json:"serial,omitempty"`
	BusPath     string   `json:"bus_path,omitempty"`
	URN         string   `json:"urn"`
	Tier        urn.Tier `json:"tier"`
}

// CardMeta holds intermediate card properties from /proc/asound/cards and /proc/asound/cardN/id.
type CardMeta struct {
	Index    int
	ID       string
	Name     string
	PCMSlots map[int]bool
}

var procCardsRegex = regexp.MustCompile(`^\s*(\d+)\s+\[([^\]]+)\]:\s+(.*)$`)

// ParseProcCards parses a Linux /proc/asound/cards text representation.
func ParseProcCards(content string) map[int]*CardMeta {
	cards := make(map[int]*CardMeta)
	scanner := bufio.NewScanner(strings.NewReader(content))
	var currentCard *CardMeta

	for scanner.Scan() {
		line := scanner.Text()
		matches := procCardsRegex.FindStringSubmatch(line)
		if len(matches) == 4 {
			idx, err := strconv.Atoi(matches[1])
			if err == nil {
				currentCard = &CardMeta{
					Index:    idx,
					ID:       strings.TrimSpace(matches[2]),
					Name:     strings.TrimSpace(matches[3]),
					PCMSlots: make(map[int]bool),
				}
				cards[idx] = currentCard
			}
		} else if currentCard != nil && strings.HasPrefix(line, " ") && currentCard.Name != "" {
			// Second line often contains human-readable device name or details
			extra := strings.TrimSpace(line)
			if extra != "" && !strings.Contains(currentCard.Name, extra) {
				currentCard.Name = currentCard.Name + " (" + extra + ")"
			}
			currentCard = nil
		}
	}

	return cards
}

// DiscoverWithRoots inspects custom sysfs and procfs directories (pure Go, zero CGO).
// Useful for Linux host discovery, container environments, and unit testing with mock fixtures.
func DiscoverWithRoots(sysRoot, procRoot string) ([]DiscoveredDevice, error) {
	cardMap := make(map[int]*CardMeta)

	// 1. Read /proc/asound/cards if available
	procCardsPath := filepath.Join(procRoot, "cards")
	if data, err := os.ReadFile(procCardsPath); err == nil {
		cardMap = ParseProcCards(string(data))
	}

	// 2. Discover cards under sysRoot (typically /sys/class/sound)
	entries, err := os.ReadDir(sysRoot)
	if err != nil && len(cardMap) == 0 {
		return nil, err
	}

	// Track discovered PCM devices per card from sysfs (e.g., pcmC0D0p, pcmC0D1c)
	pcmRegex := regexp.MustCompile(`^pcmC(\d+)D(\d+)[cp]$`)
	cardDirRegex := regexp.MustCompile(`^card(\d+)$`)

	for _, entry := range entries {
		name := entry.Name()
		if pcmMatches := pcmRegex.FindStringSubmatch(name); len(pcmMatches) == 3 {
			cIdx, err1 := strconv.Atoi(pcmMatches[1])
			dIdx, err2 := strconv.Atoi(pcmMatches[2])
			if err1 == nil && err2 == nil {
				if _, ok := cardMap[cIdx]; !ok {
					cardMap[cIdx] = &CardMeta{Index: cIdx, PCMSlots: make(map[int]bool)}
				}
				cardMap[cIdx].PCMSlots[dIdx] = true
			}
		}
	}

	var results []DiscoveredDevice

	for _, entry := range entries {
		name := entry.Name()
		cardMatches := cardDirRegex.FindStringSubmatch(name)
		if len(cardMatches) != 2 {
			continue
		}

		cardIdx, err := strconv.Atoi(cardMatches[1])
		if err != nil {
			continue
		}

		cardDir := filepath.Join(sysRoot, name)

		meta, hasMeta := cardMap[cardIdx]
		if !hasMeta {
			meta = &CardMeta{Index: cardIdx, PCMSlots: make(map[int]bool)}
			cardMap[cardIdx] = meta
		}

		// Try reading card ID from /proc/asound/cardN/id or sysRoot/cardN/id
		if meta.ID == "" {
			procIDPath := filepath.Join(procRoot, fmt.Sprintf("card%d", cardIdx), "id")
			if idBytes, err := os.ReadFile(procIDPath); err == nil {
				meta.ID = strings.TrimSpace(string(idBytes))
			} else {
				sysIDPath := filepath.Join(cardDir, "id")
				if sysIDBytes, err := os.ReadFile(sysIDPath); err == nil {
					meta.ID = strings.TrimSpace(string(sysIDBytes))
				}
			}
		}

		// Extract USB properties via symlink /sys/class/sound/cardN/device
		deviceLink := filepath.Join(cardDir, "device")
		vid, pid, serial, busPath := inspectSysfsDevice(deviceLink)

		// Determine list of subdevices
		devIndices := make([]int, 0, len(meta.PCMSlots))
		for dIdx := range meta.PCMSlots {
			devIndices = append(devIndices, dIdx)
		}
		if len(devIndices) == 0 {
			devIndices = append(devIndices, 0)
		}
		sort.Ints(devIndices)

		for _, dIdx := range devIndices {
			dev := DiscoveredDevice{
				CardIndex:   cardIdx,
				DeviceIndex: dIdx,
				CardID:      meta.ID,
				CardName:    meta.Name,
				VendorID:    vid,
				ProductID:   pid,
				Serial:      serial,
				BusPath:     busPath,
			}

			canonicalURN, tier := GenerateURN(dev)
			dev.URN = canonicalURN
			dev.Tier = tier
			results = append(results, dev)
		}
	}

	// Fallback: If sysfs was absent or lacked card entries, construct endpoints from procfs
	if len(results) == 0 && len(cardMap) > 0 {
		for cardIdx, meta := range cardMap {
			dev := DiscoveredDevice{
				CardIndex:   cardIdx,
				DeviceIndex: 0,
				CardID:      meta.ID,
				CardName:    meta.Name,
			}
			canonicalURN, tier := GenerateURN(dev)
			dev.URN = canonicalURN
			dev.Tier = tier
			results = append(results, dev)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].CardIndex != results[j].CardIndex {
			return results[i].CardIndex < results[j].CardIndex
		}
		return results[i].DeviceIndex < results[j].DeviceIndex
	})

	return results, nil
}

// inspectSysfsDevice traverses the sysfs device link to detect USB VID, PID, serial, and bus path.
func inspectSysfsDevice(deviceLink string) (vid, pid, serial, busPath string) {
	resolved, err := filepath.EvalSymlinks(deviceLink)
	if err != nil {
		// Fallback to Readlink if EvalSymlinks cannot resolve all paths
		resolved, err = os.Readlink(deviceLink)
		if err != nil {
			return "", "", "", ""
		}
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Clean(filepath.Join(filepath.Dir(deviceLink), resolved))
		}
	}

	curr := resolved
	// Ascend up to 4 parent directories looking for idVendor, idProduct, and serial
	for i := 0; i < 4 && curr != "/" && curr != "."; i++ {
		vPath := filepath.Join(curr, "idVendor")
		pPath := filepath.Join(curr, "idProduct")
		sPath := filepath.Join(curr, "serial")

		vBytes, vErr := os.ReadFile(vPath)
		pBytes, pErr := os.ReadFile(pPath)
		if vErr == nil && pErr == nil {
			vid = strings.ToLower(strings.TrimSpace(string(vBytes)))
			pid = strings.ToLower(strings.TrimSpace(string(pBytes)))

			if sBytes, sErr := os.ReadFile(sPath); sErr == nil {
				serial = strings.TrimSpace(string(sBytes))
			}

			// Bus path is derived from the canonical USB directory name or relative bus node
			busPath = filepath.Base(curr)
			break
		}
		curr = filepath.Dir(curr)
	}

	if busPath == "" && resolved != "" {
		busPath = filepath.Base(resolved)
	}

	return vid, pid, serial, busPath
}

// GenerateURN evaluates the 4-tier waterfall to construct the canonical device URN.
func GenerateURN(dev DiscoveredDevice) (string, urn.Tier) {
	// Tier 1: USB with Vendor, Product, and non-empty Serial
	if dev.VendorID != "" && dev.ProductID != "" && dev.Serial != "" {
		return urn.NewUSB(dev.VendorID, dev.ProductID, dev.Serial, dev.DeviceIndex), urn.TierUSB
	}

	// Tier 2: Sysfs Physical Bus Path for serial-less hardware
	if dev.BusPath != "" && (dev.VendorID != "" || strings.Contains(dev.BusPath, "usb") || strings.Contains(dev.BusPath, "pci")) {
		return urn.NewPath(dev.BusPath, dev.DeviceIndex), urn.TierPath
	}

	// Tier 3: ALSA Card Identifier
	if dev.CardID != "" {
		return urn.NewALSA(dev.CardID, dev.DeviceIndex), urn.TierALSA
	}

	// Tier 4: Clean Name Slug + Hash Fallback
	name := dev.CardName
	if name == "" {
		name = fmt.Sprintf("soundcard-%d", dev.CardIndex)
	}
	return urn.NewName(name), urn.TierName
}
