import type { StreamStats, FECStats, PLCStats, ASRCStats, SystemMetrics } from '$lib/api/types';
import { apiClient } from '$lib/api/client';

class TelemetryStore {
  streamStats = $state<Record<string, StreamStats>>({});
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
    try {
      const stats = await apiClient.getStreamStats();
      this.streamStats = stats || {};
    } catch {
      this.streamStats = {};
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
