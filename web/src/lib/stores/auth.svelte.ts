import { apiClient } from '$lib/api/client';

export type ConnectionStatus = 'connected' | 'connecting' | 'error' | 'unauthorized';

class AuthStore {
  authToken = $state<string>('');
  activeDaemonUrl = $state<string>('http://localhost:8000');
  connectionStatus = $state<ConnectionStatus>('connecting');
  lastError = $state<string | null>(null);
  nodeName = $state<string>('Local Node');
  version = $state<string>('1.0.0');
  uptimeSeconds = $state<number>(0);

  constructor() {
    if (typeof window !== 'undefined') {
      const savedToken = localStorage.getItem('arp2p_token');
      const savedUrl = localStorage.getItem('arp2p_url');

      if (savedToken) {
        this.authToken = savedToken;
      }
      if (savedUrl) {
        this.activeDaemonUrl = savedUrl;
      } else if (window.location.port === '5173') {
        // In Vite dev mode, use relative path so Vite proxy forwards to https://127.0.0.1:8443
        this.activeDaemonUrl = '';
      } else if (window.location.origin) {
        this.activeDaemonUrl = window.location.origin;
      }
    }

    this.applyCredentials();
  }

  setToken(token: string) {
    this.authToken = token;
    if (typeof window !== 'undefined') {
      localStorage.setItem('arp2p_token', token);
    }
    this.applyCredentials();
  }

  setDaemonUrl(url: string) {
    let clean = url.trim().replace(/\/+$/, '');
    if (clean && !clean.startsWith('http://') && !clean.startsWith('https://')) {
      clean = `http://${clean}`;
    }
    this.activeDaemonUrl = clean;
    if (typeof window !== 'undefined') {
      localStorage.setItem('arp2p_url', clean);
    }
    this.applyCredentials();
  }

  private applyCredentials() {
    apiClient.setBaseUrl(this.activeDaemonUrl);
    apiClient.setToken(this.authToken);
  }

  async checkConnection(): Promise<boolean> {
    this.connectionStatus = 'connecting';
    this.lastError = null;
    this.applyCredentials();

    try {
      const status = await apiClient.getSystemStatus();
      this.connectionStatus = 'connected';
      this.version = status.version;
      this.uptimeSeconds = status.uptime_seconds;
      return true;
    } catch (err: any) {
      if (err.status === 401 || err.code === 'ERR_UNAUTHORIZED') {
        this.connectionStatus = 'unauthorized';
        this.lastError = 'Unauthorized: Invalid or missing authentication token.';
      } else {
        this.connectionStatus = 'error';
        this.lastError = err.message || 'Unable to connect to daemon';
      }
      return false;
    }
  }
}

export const authStore = new AuthStore();
