import type {
  ClusterNodeSummary,
  AudioDeviceSummary,
  ClusterNodesResponse,
  ClusterDevicesResponse,
  NetworkHealthResponse,
  NodeHealthGrade,
  DeviceMutationRequest,
} from '$lib/api/types';
import { apiClient } from '$lib/api/client';
import { roomStore } from './rooms.svelte';
import { deviceStore } from './devices.svelte';
import { authStore } from './auth.svelte';
import { meterStore } from './meters.svelte';

class ClusterStore {
  nodes = $state<ClusterNodeSummary[]>([]);
  devices = $state<AudioDeviceSummary[]>([]);
  latencyMatrix = $state<Record<string, Record<string, number>>>({});
  networkHealth = $state<NetworkHealthResponse | null>(null);

  clusterId = $state<string>('');
  totalNodes = $state<number>(0);
  healthyNodes = $state<number>(0);
  degradedNodes = $state<number>(0);
  activeNodes = $state<number>(0);
  selectedNodeId = $state<string | null>(null);

  isLoading = $state<boolean>(false);
  error = $state<string | null>(null);
  lastUpdated = $state<Date | null>(null);

  selectedDeviceNodeFilter = $state<string>('all');
  selectedDeviceDirection = $state<'all' | 'input' | 'output' | 'virtual'>('all');

  private pollInterval: ReturnType<typeof setInterval> | null = null;

  // Derived: Local Node
  localNode = $derived.by<ClusterNodeSummary | null>(() => {
    return this.nodes.find((n) => n.is_local) || this.nodes[0] || null;
  });

  // Derived: Remote Nodes
  remoteNodes = $derived.by<ClusterNodeSummary[]>(() => {
    return this.nodes.filter((n) => !n.is_local);
  });

  // Derived: Currently selected node for detailed inspection
  selectedNode = $derived.by<ClusterNodeSummary | null>(() => {
    if (this.selectedNodeId) {
      const match = this.nodes.find((n) => (n.node_id || n.id) === this.selectedNodeId);
      if (match) return match;
    }
    return this.localNode;
  });

  // Derived: All unique node IDs
  nodeIds = $derived.by<string[]>(() => {
    return this.nodes.map((n) => n.node_id || n.id);
  });

  // Derived: Global Cluster Health Score (0 - 100)
  clusterHealthScore = $derived.by<number>(() => {
    if (this.networkHealth && typeof this.networkHealth.health_score === 'number') {
      return this.networkHealth.health_score;
    }
    if (this.nodes.length === 0) return 0;
    const sum = this.nodes.reduce((acc, n) => acc + (n.health_score ?? 100), 0);
    return Math.round((sum / this.nodes.length) * 10) / 10;
  });

  // Derived: Composite Health Grade
  clusterHealthGrade = $derived.by<NodeHealthGrade>(() => {
    const score = this.clusterHealthScore;
    if (score >= 90) return 'EXCELLENT';
    if (score >= 75) return 'GOOD';
    if (score >= 50) return 'FAIR';
    if (score >= 25) return 'POOR';
    return 'CRITICAL';
  });

  // Derived: Devices grouped by node ID
  devicesByNode = $derived.by<Record<string, AudioDeviceSummary[]>>(() => {
    const map: Record<string, AudioDeviceSummary[]> = {};
    for (const d of this.devices) {
      const nId = d.node_id || 'local';
      if (!map[nId]) map[nId] = [];
      map[nId].push(d);
    }
    return map;
  });

  // Derived: Filtered devices for DeviceManager
  filteredDevices = $derived.by<AudioDeviceSummary[]>(() => {
    let list = this.devices;

    // Filter by Node
    if (this.selectedDeviceNodeFilter === 'local') {
      const lId = this.localNode?.node_id || this.localNode?.id || 'local';
      list = list.filter((d) => d.node_id === lId || d.node_name?.toLowerCase().includes('local'));
    } else if (this.selectedDeviceNodeFilter !== 'all') {
      list = list.filter((d) => (d.node_id || d.id) === this.selectedDeviceNodeFilter);
    }

    // Filter by Direction / Kind
    if (this.selectedDeviceDirection === 'input') {
      list = list.filter((d) => d.direction === 'input' || d.direction === 'duplex' || d.max_input > 0);
    } else if (this.selectedDeviceDirection === 'output') {
      list = list.filter((d) => d.direction === 'output' || d.direction === 'duplex' || d.max_output > 0);
    } else if (this.selectedDeviceDirection === 'virtual') {
      list = list.filter((d) => d.is_virtual);
    }

    return list;
  });

  // Derived: Inputs
  inputDevices = $derived.by<AudioDeviceSummary[]>(() => {
    return this.devices.filter(
      (d) => d.direction === 'input' || d.direction === 'duplex' || d.max_input > 0
    );
  });

  // Derived: Outputs
  outputDevices = $derived.by<AudioDeviceSummary[]>(() => {
    return this.devices.filter(
      (d) => d.direction === 'output' || d.direction === 'duplex' || d.max_output > 0
    );
  });

  // Fetch cluster nodes, devices, and network health
  async pollCluster() {
    this.isLoading = true;
    try {
      const [nodesResp, devicesResp, healthResp, _roomsResp, _localDevicesResp] = await Promise.allSettled([
        apiClient.getClusterNodes(),
        apiClient.getClusterDevices(),
        apiClient.getNetworkHealth(),
        roomStore.loadRooms(true),
        deviceStore.loadDevices(undefined, true),
      ]);

      let gotNodes = false;
      let gotDevices = false;

      if (nodesResp.status === 'fulfilled' && nodesResp.value) {
        const val: ClusterNodesResponse = nodesResp.value;
        const newNodes = val.nodes || [];
        if (JSON.stringify(newNodes) !== JSON.stringify(this.nodes)) {
          this.nodes = newNodes;
        }
        this.totalNodes = val.total_nodes ?? this.nodes.length;
        this.healthyNodes = val.healthy_nodes ?? this.nodes.filter((n) => n.health_score >= 75).length;
        this.degradedNodes = val.degraded_nodes ?? (this.totalNodes - this.healthyNodes);
        this.activeNodes = val.active_nodes ?? this.nodes.filter((n) => n.swim_state === 'alive').length;
        if (val.cluster_id) this.clusterId = val.cluster_id;
        if (val.latency_matrix) this.latencyMatrix = val.latency_matrix;
        gotNodes = true;
      }

      if (devicesResp.status === 'fulfilled' && devicesResp.value) {
        const val: ClusterDevicesResponse = devicesResp.value;
        const newDevices = val.devices || [];
        if (JSON.stringify(newDevices) !== JSON.stringify(this.devices)) {
          this.devices = newDevices;
        }
        gotDevices = true;
      }

      if (healthResp.status === 'fulfilled' && healthResp.value) {
        this.networkHealth = healthResp.value;
        if (!this.latencyMatrix || Object.keys(this.latencyMatrix).length === 0) {
          if (healthResp.value.latency_matrix) {
            const localId = healthResp.value.node_id || 'node-local';
            this.latencyMatrix = {
              [localId]: healthResp.value.latency_matrix,
            };
          }
        }
      }

      if (!gotNodes) {
        this.nodes = [];
        this.devices = [];
        this.totalNodes = 0;
        this.healthyNodes = 0;
        this.degradedNodes = 0;
        this.activeNodes = 0;
        const reason = nodesResp.status === 'rejected' ? (nodesResp.reason?.message || 'Failed to poll cluster') : 'Failed to poll cluster';
        this.error = reason;
      } else {
        this.error = null;
        this.lastUpdated = new Date();
        // When gateway connectivity is established/restored, ensure auth status & meter transport
        if (authStore.connectionStatus !== 'connected') {
          await authStore.checkConnection(true);
        }
        meterStore.ensureConnected();
      }
    } catch (err: any) {
      this.error = err.message || 'Failed to poll cluster';
      this.nodes = [];
      this.devices = [];
      this.totalNodes = 0;
      this.healthyNodes = 0;
      this.degradedNodes = 0;
      this.activeNodes = 0;
    } finally {
      this.isLoading = false;
    }
  }

  // Update remote or local device volume / mute via atomic proxy
  async patchDevice(nodeId: string, deviceId: string, updates: DeviceMutationRequest) {
    // 1. Optimistic Update on all matching devices
    const prevDevices = JSON.parse(JSON.stringify(this.devices));
    const targetDevs = this.devices.filter(
      (d) => (d.id === deviceId || d.device_id === deviceId) && (d.node_id === nodeId || !nodeId)
    );

    for (const dev of targetDevs) {
      if (updates.volume !== undefined) dev.volume = updates.volume;
      if (updates.muted !== undefined) dev.muted = updates.muted;
    }

    // Also update inside the nodes hierarchy
    for (const node of this.nodes) {
      if (node.id === nodeId || node.node_id === nodeId) {
        for (const dev of node.devices) {
          if (dev.id === deviceId || dev.device_id === deviceId) {
            if (updates.volume !== undefined) dev.volume = updates.volume;
            if (updates.muted !== undefined) dev.muted = updates.muted;
          }
        }
      }
    }

    // 2. Dispatch to backend
    try {
      const resolvedNodeId = nodeId || this.localNode?.id || 'local';
      await apiClient.patchClusterDevice(resolvedNodeId, deviceId, updates);
    } catch (err: any) {
      // Rollback on network failure
      this.devices = prevDevices;
      this.error = `Failed to update device ${deviceId} on node ${nodeId}: ${err.message}`;
      throw err;
    }
  }

  selectNode(nodeId: string | null) {
    this.selectedNodeId = nodeId;
  }

  setDeviceFilter(nodeFilter: string, direction?: 'all' | 'input' | 'output' | 'virtual') {
    this.selectedDeviceNodeFilter = nodeFilter;
    if (direction) {
      this.selectedDeviceDirection = direction;
    }
  }

  startPolling(intervalMs = 2500) {
    this.stopPolling();
    this.pollCluster();
    this.pollInterval = setInterval(() => {
      this.pollCluster();
    }, intervalMs);
  }

  stopPolling() {
    if (this.pollInterval) {
      clearInterval(this.pollInterval);
      this.pollInterval = null;
    }
  }
}

export const clusterStore = new ClusterStore();
