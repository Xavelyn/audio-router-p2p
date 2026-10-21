import type { MeterFrame, ChannelMeter, DTXState } from '$lib/api/types';
import { SSEManager } from '$lib/api/sse';
import { TelemetryWebSocket } from '$lib/api/ws';
import { authStore } from './auth.svelte';

export interface ChannelState {
  channel: number;
  rmsDb: number;
  peakDb: number;
  peakHoldDb: number;
  clipped: boolean;
  clipHoldUntil: number;
  lastUpdated: number;
}

export interface StreamMeterState {
  streamId: string;
  dtxState: DTXState;
  timestampNs: number;
  channels: ChannelState[];
}

class MeterStore {
  streams = $state<Record<string, StreamMeterState>>({});
  activeChannelMeters = $state<ChannelState[]>([]);
  useSimulation = $state<boolean>(false);
  isConnected = $state<boolean>(false);

  private sse: SSEManager | null = null;
  private ws: TelemetryWebSocket | null = null;
  private lastRealPacketTime = 0;
  private animFrameId: number | null = null;
  private simTimer: ReturnType<typeof setInterval> | null = null;

  constructor() {
    this.startDecayLoop();
  }

  public init() {
    this.setupTransport();
  }

  public setupTransport() {
    // Teardown existing
    if (this.ws) {
      this.ws.disconnect();
      this.ws = null;
    }
    if (this.sse) {
      this.sse.disconnect();
      this.sse = null;
    }

    const baseUrl = authStore.activeDaemonUrl;
    const token = authStore.authToken;

    // Prefer WebSocket for low-latency bidirectional telemetry
    this.ws = new TelemetryWebSocket({ baseUrl, token });
    this.ws.onStateChange((state) => {
      this.isConnected = state === 'connected';
      // If WebSocket fails or errors, fallback to SSE
      if (state === 'error' && (!this.sse || this.sse.state === 'disconnected')) {
        this.fallbackToSSE(baseUrl, token);
      }
    });

    this.ws.onMeter((frame: MeterFrame) => {
      this.handleMeterFrame(frame);
    });

    this.ws.connect();
  }

  private fallbackToSSE(baseUrl: string, token: string) {
    if (this.sse) return;
    this.sse = new SSEManager('/api/v1/telemetry/meters', { baseUrl, token, rate: 30 });
    this.sse.onStateChange((st) => {
      if (st === 'connected') this.isConnected = true;
    });
    this.sse.onMeter((frame: MeterFrame) => {
      this.handleMeterFrame(frame);
    });
    this.sse.connect();
  }

  public handleMeterFrame(frame: MeterFrame) {
    this.lastRealPacketTime = Date.now();
    const now = Date.now();

    const existingStream = this.streams[frame.stream_id];
    const prevChannels = existingStream?.channels || [];

    const updatedChannels: ChannelState[] = frame.channels.map((ch) => {
      const prev = prevChannels.find((p) => p.channel === ch.channel);
      const prevPeakHold = prev?.peakHoldDb ?? -96;
      const prevClipHold = prev?.clipHoldUntil ?? 0;

      // Peak hold logic: hold peak if new peak is higher, or slowly drop
      const newPeak = ch.peak_dbfs;
      let peakHold = Math.max(newPeak, prevPeakHold);

      // Clip indicator hold (light stays lit for 1.2s on clip)
      let clipHold = prevClipHold;
      if (ch.clipped || ch.peak_dbfs >= -0.1) {
        clipHold = now + 1200;
      }

      return {
        channel: ch.channel,
        rmsDb: ch.rms_dbfs,
        peakDb: ch.peak_dbfs,
        peakHoldDb: peakHold,
        clipped: now < clipHold,
        clipHoldUntil: clipHold,
        lastUpdated: now,
      };
    });

    this.streams[frame.stream_id] = {
      streamId: frame.stream_id,
      dtxState: frame.dtx_state,
      timestampNs: frame.timestamp_ns,
      channels: updatedChannels,
    };

    // Flatten channels for primary rack view
    this.recomputeActiveChannels();
  }

  private recomputeActiveChannels() {
    const streamKeys = Object.keys(this.streams);
    if (streamKeys.length > 0) {
      // Show first active stream channels, or combine
      const primaryStream = this.streams[streamKeys[0]];
      this.activeChannelMeters = primaryStream.channels;
    } else if (this.useSimulation) {
      // Simulation will populate activeChannelMeters
    } else {
      // Default idle stereo meters
      this.activeChannelMeters = [
        { channel: 0, rmsDb: -96, peakDb: -96, peakHoldDb: -96, clipped: false, clipHoldUntil: 0, lastUpdated: Date.now() },
        { channel: 1, rmsDb: -96, peakDb: -96, peakHoldDb: -96, clipped: false, clipHoldUntil: 0, lastUpdated: Date.now() },
      ];
    }
  }

  // Decay loop running at ~60fps to decay peak needle hold & clip lamps
  private startDecayLoop() {
    let lastTick = performance.now();

    const tick = (time: number) => {
      const dt = (time - lastTick) / 1000;
      lastTick = time;
      const now = Date.now();

      // Check if real stream is silent or not sending
      if (now - this.lastRealPacketTime > 2000 && !this.useSimulation && Object.keys(this.streams).length === 0) {
        // Idle decay
      }

      for (const stream of Object.values(this.streams)) {
        for (const ch of stream.channels) {
          // Peak hold decay: ~20 dB per second
          if (ch.peakHoldDb > ch.peakDb) {
            ch.peakHoldDb = Math.max(ch.peakDb, ch.peakHoldDb - 22 * dt);
          }
          if (now >= ch.clipHoldUntil) {
            ch.clipped = false;
          }
        }
      }

      for (const ch of this.activeChannelMeters) {
        if (ch.peakHoldDb > ch.peakDb) {
          ch.peakHoldDb = Math.max(ch.peakDb, ch.peakHoldDb - 22 * dt);
        }
        if (now >= ch.clipHoldUntil) {
          ch.clipped = false;
        }
      }

      this.animFrameId = requestAnimationFrame(tick);
    };

    if (typeof window !== 'undefined') {
      this.animFrameId = requestAnimationFrame(tick);
    }
  }

  public toggleSimulation(enable?: boolean) {
    this.useSimulation = enable !== undefined ? enable : !this.useSimulation;

    if (this.useSimulation) {
      if (!this.simTimer) {
        let phase = 0;
        this.simTimer = setInterval(() => {
          phase += 0.15;
          const now = Date.now();
          const numChannels = 4;
          const fakeChannels: ChannelState[] = [];

          for (let i = 0; i < numChannels; i++) {
            const mod = Math.sin(phase + i * 1.2) * 0.5 + 0.5;
            const beat = Math.pow(Math.sin(phase * 2 + i * 0.7), 4);
            const peakVal = -40 + mod * 36 + beat * 4; // -40 to 0 dBFS
            const rmsVal = peakVal - (6 + Math.random() * 4);
            const willClip = peakVal > -0.5 && Math.random() > 0.85;

            fakeChannels.push({
              channel: i,
              rmsDb: rmsVal,
              peakDb: willClip ? 0.0 : peakVal,
              peakHoldDb: willClip ? 0.0 : Math.max(peakVal, -18),
              clipped: willClip,
              clipHoldUntil: willClip ? now + 1200 : 0,
              lastUpdated: now,
            });
          }

          this.activeChannelMeters = fakeChannels;
          this.streams['demo-multitrack-bus'] = {
            streamId: 'demo-multitrack-bus',
            dtxState: 'Active',
            timestampNs: Date.now() * 1000000,
            channels: fakeChannels,
          };
        }, 33); // ~30Hz
      }
    } else {
      if (this.simTimer) {
        clearInterval(this.simTimer);
        this.simTimer = null;
      }
      delete this.streams['demo-multitrack-bus'];
      this.recomputeActiveChannels();
    }
  }

  public destroy() {
    if (this.animFrameId) {
      cancelAnimationFrame(this.animFrameId);
    }
    if (this.simTimer) {
      clearInterval(this.simTimer);
    }
    if (this.ws) {
      this.ws.disconnect();
    }
    if (this.sse) {
      this.sse.disconnect();
    }
  }
}

export const meterStore = new MeterStore();
