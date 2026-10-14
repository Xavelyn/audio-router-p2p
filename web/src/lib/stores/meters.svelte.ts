import type { MeterFrame, ChannelMeter, DTXState } from '$lib/api/types';
import { SSEManager } from '$lib/api/sse';
import { TelemetryWebSocket } from '$lib/api/ws';
import { authStore } from './auth.svelte';

// Displayed-level ballistics (studio VU behavior): fast attack, slow release.
// Smoothing is applied once when a meter frame is written into the store;
// all consumers (bars, needles, readouts) read the already-smoothed values.
const SILENCE_FLOOR_DB = -96;
const METER_ATTACK_MS = 40;
const METER_RELEASE_MS = 400;

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
  lastUpdated: number;
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

  public ensureConnected() {
    if (!this.isConnected && (!this.ws || this.ws.state === 'disconnected' || this.ws.state === 'error')) {
      this.setupTransport();
    }
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

  // Exponential ballistics filter for displayed dB values: rises fast when the
  // level increases (attack), falls slowly when it decreases (release). dtMs is
  // the time since the previous write for this channel, so the smoothing is
  // independent of the actual frame rate.
  private smoothLevel(prev: number | undefined, next: number, dtMs: number): number {
    if (prev === undefined || !Number.isFinite(prev) || !Number.isFinite(next)) return next;
    if (next === prev) return next;
    const tau = next > prev ? METER_ATTACK_MS : METER_RELEASE_MS;
    const alpha = dtMs <= 0 ? 1 : Math.min(1, 1 - Math.exp(-dtMs / tau));
    const out = prev + (next - prev) * alpha;
    return out < SILENCE_FLOOR_DB ? SILENCE_FLOOR_DB : out;
  }

  public handleMeterFrame(frame: MeterFrame) {
    if (frame.closed) {
      if (this.streams[frame.stream_id]) {
        delete this.streams[frame.stream_id];
        this.recomputeActiveChannels();
      }
      return;
    }

    const now = Date.now();
    this.lastRealPacketTime = now;

    const existingStream = this.streams[frame.stream_id];
    const prevChannels = existingStream?.channels || [];

    const updatedChannels: ChannelState[] = (frame.channels || []).map((ch) => {
      const prev = prevChannels.find((p) => p.channel === ch.channel);
      const prevPeakHold = prev?.peakHoldDb ?? -96;
      const prevClipHold = prev?.clipHoldUntil ?? 0;
      const dtMs = prev ? Math.max(0, now - prev.lastUpdated) : 0;

      // Smooth the displayed level with attack/release ballistics.
      const rmsDb = this.smoothLevel(prev?.rmsDb, ch.rms_dbfs, dtMs);
      const peakDb = this.smoothLevel(prev?.peakDb, ch.peak_dbfs, dtMs);

      // Peak hold logic: hold peak if new peak is higher, or slowly drop
      const peakHold = Math.max(peakDb, prevPeakHold);

      // Clip indicator hold (light stays lit for 1.2s on clip)
      let clipHold = prevClipHold;
      if (ch.clipped || ch.peak_dbfs >= -0.1) {
        clipHold = now + 1200;
      }

      return {
        channel: ch.channel,
        rmsDb,
        peakDb,
        peakHoldDb: peakHold,
        clipped: now < clipHold,
        clipHoldUntil: clipHold,
        lastUpdated: now,
      };
    });

    this.streams[frame.stream_id] = {
      streamId: frame.stream_id,
      dtxState: frame.dtx_state || 'Active',
      timestampNs: frame.timestamp_ns || (now * 1000000),
      channels: updatedChannels,
      lastUpdated: now,
    };

    // Flatten channels for primary rack view
    this.recomputeActiveChannels();
  }

  private recomputeActiveChannels() {
    const now = Date.now();
    // Filter active streams updated within 1500ms
    const activeStreams = Object.values(this.streams).filter(
      (s) => s.streamId === 'demo-multitrack-bus' || (now - s.lastUpdated < 1500)
    );

    if (activeStreams.length > 0) {
      // Pick the most recently updated active stream
      activeStreams.sort((a, b) => b.lastUpdated - a.lastUpdated);
      this.activeChannelMeters = activeStreams[0].channels;
    } else if (this.useSimulation) {
      // Simulation will populate activeChannelMeters
    } else {
      // Default idle stereo meters
      this.activeChannelMeters = [
        { channel: 0, rmsDb: -96, peakDb: -96, peakHoldDb: -96, clipped: false, clipHoldUntil: 0, lastUpdated: now },
        { channel: 1, rmsDb: -96, peakDb: -96, peakHoldDb: -96, clipped: false, clipHoldUntil: 0, lastUpdated: now },
      ];
    }
  }

  // Decay loop running at ~60fps to decay peak needle hold & clip lamps
  private startDecayLoop() {
    let lastTick = typeof performance !== 'undefined' ? performance.now() : Date.now();

    const tick = (time: number) => {
      const dt = Math.min(Math.max((time - lastTick) / 1000, 0), 0.1);
      lastTick = time;
      const now = Date.now();
      let streamEvicted = false;

      for (const [sId, stream] of Object.entries(this.streams)) {
        if (sId === 'demo-multitrack-bus') continue;

        const ageMs = now - stream.lastUpdated;

        // TTL eviction after 1500ms without frames
        if (ageMs > 1500) {
          delete this.streams[sId];
          streamEvicted = true;
          continue;
        }

        // If packets paused (>100ms), decay audio levels towards noise floor (-96 dBFS)
        if (ageMs > 100) {
          for (const ch of stream.channels) {
            ch.peakDb = Math.max(-96, ch.peakDb - 24 * dt);
            ch.rmsDb = Math.max(-96, ch.rmsDb - 24 * dt);
          }
        }

        // Peak hold decay: ~20 dB per second
        for (const ch of stream.channels) {
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

      if (streamEvicted) {
        this.recomputeActiveChannels();
      }

      if (typeof window !== 'undefined' && typeof window.requestAnimationFrame === 'function') {
        this.animFrameId = requestAnimationFrame(tick);
      }
    };

    if (typeof window !== 'undefined' && typeof window.requestAnimationFrame === 'function') {
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
