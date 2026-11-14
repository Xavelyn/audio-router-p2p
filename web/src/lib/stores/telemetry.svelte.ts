import type { StreamStats, FECStats, PLCStats, ASRCStats, SystemMetrics, DeviceHealth } from '$lib/api/types';
import { apiClient } from '$lib/api/client';

class TelemetryStore {
  streamStats = $state<Record<string, StreamStats>>({});
  /**
   * ADR 0034: URN-keyed per-device artifact health from
   * GET /api/v1/telemetry/device-artifacts. Empty object when the daemon's
   * ArtifactAggregator is unwired (byte-compatible fallback).
   */
  deviceArtifacts = $state<Record<string, DeviceHealth>>({});
  /**
   * Epoch-ms of the last successful telemetry tick (either endpoint).
   * 0 = never fetched. Surfaces render nothing when this is stale
   * (1500 ms convention, ADR 0026/0034) — "no data" must never look clean.
   */
  lastUpdated = $state(0);
  systemMetrics = $state<SystemMetrics>({
    cpu_percent: 0,
    memory_used_mb: 0,
    memory_total_mb: 0,
    goroutines: 0,
    gc_pause_ns: 0,
  });

  // Aggregate metrics across all streams
  totalPacketsSent = $derived(
    Object.values(this.streamStats).reduce((acc, s) => acc + (s.packets_sent || 0), 0)
  );

  totalPacketsReceived = $derived(
    Object.values(this.streamStats).reduce((acc, s) => acc + (s.packets_received || 0), 0)
  );

  totalFecRecovered = $derived(
    Object.values(this.streamStats).reduce((acc, s) => acc + (s.fec?.packets_recovered_total || 0), 0)
  );

  totalFecUnrecoverable = $derived(
    Object.values(this.streamStats).reduce((acc, s) => acc + (s.fec?.unrecoverable_groups_total || 0), 0)
  );

  totalPlcConcealed = $derived(
    Object.values(this.streamStats).reduce((acc, s) => acc + (s.plc?.concealed_frames_total || 0), 0)
  );

  // Primary stream for detailed gauges
  primaryStream = $derived.by<StreamStats | null>(() => {
    const list = Object.values(this.streamStats);
    return list.length > 0 ? list[0] : null;
  });

  isLoading = $state<boolean>(false);
  error = $state<string | null>(null);

  private pollInterval: ReturnType<typeof setInterval> | null = null;

  async fetchTelemetry() {
    // ADR 0034: device-artifacts piggybacks on the existing stream-stats
    // polling tick (no separate timer). The two fetches settle
    // independently so one endpoint failing never blanks the other.
    const [stats, artifacts] = await Promise.allSettled([
      apiClient.getStreamStats(),
      apiClient.getDeviceArtifacts(),
    ]);

    this.streamStats = stats.status === 'fulfilled' ? stats.value || {} : {};
    this.deviceArtifacts = artifacts.status === 'fulfilled' ? artifacts.value || {} : {};

    if (stats.status === 'fulfilled' || artifacts.status === 'fulfilled') {
      this.lastUpdated = Date.now();
    }
  }

  public startPolling(intervalMs = 2000) {
    this.stopPolling();
    this.fetchTelemetry();
    this.pollInterval = setInterval(() => {
      this.fetchTelemetry();
    }, intervalMs);
  }

  public stopPolling() {
    if (this.pollInterval) {
      clearInterval(this.pollInterval);
      this.pollInterval = null;
    }
  }
}

export const telemetryStore = new TelemetryStore();
