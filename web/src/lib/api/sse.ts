import type { MeterFrame } from './types';

export type SSEConnectionState = 'connected' | 'connecting' | 'disconnected' | 'error';

export interface SSEOptions {
  baseUrl?: string;
  token?: string;
  rate?: number; // 10 to 60 Hz
  reconnectIntervalMs?: number;
  maxReconnectIntervalMs?: number;
}

export type SSEEventHandler<T = unknown> = (data: T) => void;

export class SSEManager {
  private eventSource: EventSource | null = null;
  private endpoint: string;
  private baseUrl: string = '';
  private token: string = '';
  private rate: number = 30;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectIntervalMs: number = 1000;
  private maxReconnectIntervalMs: number = 10000;
  private currentBackoff: number = 1000;
  private isExplicitlyClosed: boolean = false;
  private listeners: Map<string, Set<SSEEventHandler>> = new Map();
  private stateChangeListeners: Set<(state: SSEConnectionState) => void> = new Set();
  public state: SSEConnectionState = 'disconnected';

  constructor(endpoint: string = '/api/v1/telemetry/meters', options: SSEOptions = {}) {
    this.endpoint = endpoint;
    this.baseUrl = (options.baseUrl || '').replace(/\/+$/, '');
    this.token = options.token || '';
    this.rate = options.rate || 30;
    this.reconnectIntervalMs = options.reconnectIntervalMs || 1000;
    this.maxReconnectIntervalMs = options.maxReconnectIntervalMs || 10000;
    this.currentBackoff = this.reconnectIntervalMs;
  }

  setBaseUrl(url: string) {
    this.baseUrl = url.replace(/\/+$/, '');
    if (this.eventSource) {
      this.reconnect();
    }
  }

  setToken(token: string) {
    this.token = token;
    if (this.eventSource) {
      this.reconnect();
    }
  }

  setRate(rate: number) {
    this.rate = Math.max(10, Math.min(60, rate));
    if (this.eventSource) {
      this.reconnect();
    }
  }

  public connect(): void {
    if (this.eventSource && (this.eventSource.readyState === EventSource.OPEN || this.eventSource.readyState === EventSource.CONNECTING)) {
      return;
    }

    this.isExplicitlyClosed = false;
    this.setState('connecting');

    const params = new URLSearchParams();
    if (this.token) params.set('token', this.token);
    if (this.rate) params.set('rate', this.rate.toString());

    const queryString = params.toString() ? `?${params.toString()}` : '';
    const fullUrl = `${this.baseUrl}${this.endpoint}${queryString}`;

    try {
      this.eventSource = new EventSource(fullUrl);

      this.eventSource.onopen = () => {
        this.currentBackoff = this.reconnectIntervalMs;
        this.setState('connected');
      };

      this.eventSource.onerror = () => {
        this.setState('error');
        if (this.eventSource) {
          this.eventSource.close();
          this.eventSource = null;
        }

        if (!this.isExplicitlyClosed) {
          this.scheduleReconnect();
        }
      };

      // Handle standard message
      this.eventSource.onmessage = (event) => {
        try {
          const parsed = JSON.parse(event.data);
          this.emit('message', parsed);
        } catch {
          this.emit('message', event.data);
        }
      };

      // Handle custom events
      const knownEvents = ['meter', 'route_change', 'peer_change', 'device_change', 'clock_sync'];
      for (const evName of knownEvents) {
        this.eventSource.addEventListener(evName, (event: MessageEvent) => {
          try {
            const parsed = JSON.parse(event.data);
            this.emit(evName, parsed);
          } catch {
            this.emit(evName, event.data);
          }
        });
      }
    } catch {
      this.setState('error');
      this.scheduleReconnect();
    }
  }

  public disconnect(): void {
    this.isExplicitlyClosed = true;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
    this.setState('disconnected');
  }

  public reconnect(): void {
    this.disconnect();
    this.isExplicitlyClosed = false;
    this.connect();
  }

  private scheduleReconnect(): void {
    if (this.reconnectTimer || this.isExplicitlyClosed) return;

    this.setState('connecting');
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, this.currentBackoff);

    // Exponential backoff with ceiling
    this.currentBackoff = Math.min(this.currentBackoff * 1.5, this.maxReconnectIntervalMs);
  }

  private setState(newState: SSEConnectionState): void {
    if (this.state !== newState) {
      this.state = newState;
      for (const listener of this.stateChangeListeners) {
        listener(newState);
      }
    }
  }

  public onStateChange(listener: (state: SSEConnectionState) => void): () => void {
    this.stateChangeListeners.add(listener);
    listener(this.state);
    return () => this.stateChangeListeners.delete(listener);
  }

  public on<T = unknown>(eventName: string, handler: SSEEventHandler<T>): () => void {
    if (!this.listeners.has(eventName)) {
      this.listeners.set(eventName, new Set());
      if (this.eventSource && !['message', 'meter', 'route_change', 'peer_change', 'device_change', 'clock_sync'].includes(eventName)) {
        this.eventSource.addEventListener(eventName, (event: MessageEvent) => {
          try {
            const parsed = JSON.parse(event.data);
            this.emit(eventName, parsed);
          } catch {
            this.emit(eventName, event.data);
          }
        });
      }
    }

    const set = this.listeners.get(eventName)!;
    set.add(handler as SSEEventHandler);

    return () => {
      set.delete(handler as SSEEventHandler);
    };
  }

  public onMeter(handler: (frame: MeterFrame) => void): () => void {
    return this.on<MeterFrame>('meter', handler);
  }

  private emit(eventName: string, data: unknown): void {
    const handlers = this.listeners.get(eventName);
    if (handlers) {
      for (const handler of handlers) {
        try {
          handler(data);
        } catch (err) {
          console.error(`Error in SSE listener for "${eventName}":`, err);
        }
      }
    }
  }
}
