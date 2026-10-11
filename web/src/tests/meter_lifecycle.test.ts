import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { meterStore } from '$lib/stores/meters.svelte';
import { urnChecksum } from '$lib/utils/checksum';
import type { MeterFrame } from '$lib/api/types';

describe('ADR-0026: Meter Telemetry Lifecycle and Deterministic Binding', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    // Clear any streams
    for (const key of Object.keys(meterStore.streams)) {
      delete meterStore.streams[key];
    }
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  describe('urnChecksum', () => {
    it('generates deterministic 8-character hex checksum matching Go urn.Checksum', () => {
      expect(urnChecksum('Emulated Microphone')).toBe('0652b7fa');
      expect(urnChecksum('Emulated Speaker')).toBe('7e5ccbf2');
      expect(urnChecksum('urn:audio:virt:vdev-1')).toBe('4863eab0');
      expect(urnChecksum('hw:0,0')).toBe('d7d37d44');
      expect(urnChecksum('urn:audio:hw:name:emulated-microphone:0652b7fa')).toBe('85310a64');
    });

    it('returns empty string for empty input', () => {
      expect(urnChecksum('')).toBe('');
    });
  });

  describe('MeterStore lifecycle and tombstones', () => {
    it('ingests active meter frame and populates stream state with timestamp', () => {
      const frame: MeterFrame = {
        stream_id: 'stream-alpha',
        timestamp_ns: Date.now() * 1000000,
        dtx_state: 'Active',
        channels: [
          { channel: 0, rms_dbfs: -18.5, peak_dbfs: -12.0, clipped: false },
          { channel: 1, rms_dbfs: -20.1, peak_dbfs: -14.2, clipped: false },
        ],
      };

      meterStore.handleMeterFrame(frame);

      expect(meterStore.streams['stream-alpha']).toBeDefined();
      expect(meterStore.streams['stream-alpha'].channels[0].peakDb).toBe(-12.0);
      expect(meterStore.streams['stream-alpha'].channels[1].rmsDb).toBe(-20.1);
      expect(meterStore.streams['stream-alpha'].lastUpdated).toBeGreaterThan(0);
      expect(meterStore.activeChannelMeters[0].peakDb).toBe(-12.0);
    });

    it('immediately purges stream upon receiving closed tombstone frame', () => {
      // Ingest live stream
      meterStore.handleMeterFrame({
        stream_id: 'stream-to-close',
        timestamp_ns: Date.now() * 1000000,
        dtx_state: 'Active',
        channels: [
          { channel: 0, rms_dbfs: -10, peak_dbfs: -6, clipped: false },
        ],
      });
      expect(meterStore.streams['stream-to-close']).toBeDefined();

      // Receive tombstone frame from backend
      meterStore.handleMeterFrame({
        stream_id: 'stream-to-close',
        channels: [],
        closed: true,
      });

      // Stream must be deleted immediately
      expect(meterStore.streams['stream-to-close']).toBeUndefined();
      // activeChannelMeters should reset to idle (-96 dBFS)
      expect(meterStore.activeChannelMeters[0].peakDb).toBe(-96);
    });

    it('picks the most recently updated stream on recreation', () => {
      // Old stream started at T = 1000
      vi.setSystemTime(1000);
      meterStore.handleMeterFrame({
        stream_id: 'room-alpha-input1-out1',
        timestamp_ns: 1000 * 1000000,
        dtx_state: 'Active',
        channels: [
          { channel: 0, rms_dbfs: -30, peak_dbfs: -24, clipped: false },
        ],
      });

      // Recreated stream started at T = 2000
      vi.setSystemTime(2000);
      meterStore.handleMeterFrame({
        stream_id: 'room-alpha-input1-out2',
        timestamp_ns: 2000 * 1000000,
        dtx_state: 'Active',
        channels: [
          { channel: 0, rms_dbfs: -12, peak_dbfs: -6, clipped: false },
        ],
      });

      // activeChannelMeters must select the newer stream (-6 dB peak, not -24 dB)
      expect(meterStore.activeChannelMeters[0].peakDb).toBe(-6);
    });

    it('ingests and tracks multi-node cluster streams and distinct :out ingress taps', () => {
      vi.setSystemTime(5000);
      // Source node emits input stream meter frame
      meterStore.handleMeterFrame({
        stream_id: 'room-conf-nodeA-mic1-nodeB-spk1',
        timestamp_ns: 5000 * 1000000,
        dtx_state: 'Active',
        channels: [
          { channel: 0, rms_dbfs: -18.0, peak_dbfs: -12.0, clipped: false },
        ],
      });

      // Destination node emits ingress output stream meter frame
      meterStore.handleMeterFrame({
        stream_id: 'room-conf-nodeA-mic1-nodeB-spk1:out',
        timestamp_ns: 5000 * 1000000,
        dtx_state: 'Active',
        channels: [
          { channel: 0, rms_dbfs: -20.0, peak_dbfs: -14.0, clipped: false },
        ],
      });

      // Both must exist simultaneously without overwriting
      expect(meterStore.streams['room-conf-nodeA-mic1-nodeB-spk1']).toBeDefined();
      expect(meterStore.streams['room-conf-nodeA-mic1-nodeB-spk1:out']).toBeDefined();

      expect(meterStore.streams['room-conf-nodeA-mic1-nodeB-spk1'].channels[0].peakDb).toBe(-12.0);
      expect(meterStore.streams['room-conf-nodeA-mic1-nodeB-spk1:out'].channels[0].peakDb).toBe(-14.0);
    });
  });
});
