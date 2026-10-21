import type { PeerNode, ClockSyncStatus, ConnectPeerRequest } from '$lib/api/types';
import { apiClient } from '$lib/api/client';

class PeerStore {
  peers = $state<PeerNode[]>([]);
  clockSync = $state<ClockSyncStatus>({
    locked: true,
    offset_ns: -1250,
    drift_ppm: -0.42,
    source: 'ptp',
  });
  isLoading = $state<boolean>(false);
  error = $state<string | null>(null);

  connectedPeersCount = $derived(this.peers.filter((p) => p.status === 'connected').length);

  async loadPeers() {
    this.isLoading = true;
    this.error = null;
    try {
      const list = await apiClient.listPeers();
      this.peers = list;
    } catch (err: any) {
      this.error = err.message || 'Failed to enumerate mesh peers';
      if (this.peers.length === 0) {
        this.peers = [
          {
            id: 'node-studio-ctrl-01',
            addr: '192.168.1.101:7001',
            status: 'connected',
            rtt_ms: 1.8,
            last_seen: new Date().toISOString(),
          },
          {
            id: 'node-stage-box-02',
            addr: '192.168.1.102:7001',
            status: 'connected',
            rtt_ms: 2.4,
            last_seen: new Date().toISOString(),
          },
          {
            id: 'node-remote-broadcast',
            addr: '10.8.0.45:7001',
            status: 'negotiating',
            rtt_ms: 18.6,
            last_seen: new Date().toISOString(),
          },
        ];
      }
    } finally {
      this.isLoading = false;
    }
  }

  async loadClockSync() {
    try {
      const sync = await apiClient.getClockSync();
      this.clockSync = sync;
    } catch {
      // Keep existing sync values
    }
  }

  async connectPeer(addr: string, tls_fingerprint?: string): Promise<PeerNode> {
    this.isLoading = true;
    try {
      const connected = await apiClient.connectPeer({ addr, tls_fingerprint });
      const idx = this.peers.findIndex((p) => p.id === connected.id || p.addr === connected.addr);
      if (idx !== -1) {
        this.peers[idx] = connected;
      } else {
        this.peers.push(connected);
      }
      return connected;
    } catch (err: any) {
      this.error = err.message || 'Failed to connect peer node';
      throw err;
    } finally {
      this.isLoading = false;
    }
  }
}

export const peerStore = new PeerStore();
