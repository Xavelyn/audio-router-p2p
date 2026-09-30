import type {
  SystemStatus,
  PeerNode,
  ConnectPeerRequest,
  ClockSyncStatus,
  AudioDevice,
  CreateVirtualDeviceRequest,
  UpdateDeviceRequest,
  MatrixRoute,
  CreateRouteRequest,
  PatchRouteRequest,
  CrosspointGainRequest,
  RoomInfo,
  CreateRoomRequest,
  UpdateRoomBindingsRequest,
  StreamStats,
  ErrorResponse,
  NetworkHealthResponse,
  ClusterNodesResponse,
  ClusterDevicesResponse,
  DeviceMutationRequest,
  DeviceMutationResponse,
} from './types';

export class ApiError extends Error {
  code: string;
  status: number;
  details?: Record<string, unknown>;

  constructor(message: string, code: string, status: number, details?: Record<string, unknown>) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.status = status;
    this.details = details;
  }
}

export interface ApiClientConfig {
  baseUrl?: string;
  token?: string;
}

export class ApiClient {
  private baseUrl: string;
  private token: string;

  constructor(config: ApiClientConfig = {}) {
    this.baseUrl = (config.baseUrl || '').replace(/\/+$/, '');
    this.token = config.token || '';
  }

  setBaseUrl(url: string) {
    this.baseUrl = url.replace(/\/+$/, '');
  }

  setToken(token: string) {
    this.token = token;
  }

  getBaseUrl(): string {
    return this.baseUrl;
  }

  getToken(): string {
    return this.token;
  }

  private async request<T>(path: string, options: RequestInit = {}): Promise<T> {
    const url = `${this.baseUrl}${path.startsWith('/') ? path : `/${path}`}`;
    const headers = new Headers(options.headers || {});

    if (!headers.has('Content-Type') && options.body && typeof options.body === 'string') {
      headers.set('Content-Type', 'application/json');
    }

    if (this.token) {
      headers.set('Authorization', `Bearer ${this.token}`);
      headers.set('X-Auth-Token', this.token);
    }

    let response: Response;
    try {
      response = await fetch(url, {
        ...options,
        headers,
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Network request failed';
      throw new ApiError(msg, 'ERR_NETWORK', 0);
    }

    if (!response.ok) {
      let errJson: ErrorResponse | null = null;
      try {
        errJson = await response.json();
      } catch {
        // Response was not JSON
      }

      if (errJson && errJson.error) {
        throw new ApiError(
          errJson.error,
          errJson.code || `ERR_HTTP_${response.status}`,
          response.status,
          errJson.details
        );
      }

      throw new ApiError(
        response.statusText || `HTTP request failed with status ${response.status}`,
        `ERR_HTTP_${response.status}`,
        response.status
      );
    }

    if (response.status === 204) {
      return {} as T;
    }

    return (await response.json()) as T;
  }

  // ==========================================================================
  // System Management
  // ==========================================================================

  async getSystemStatus(): Promise<SystemStatus> {
    return this.request<SystemStatus>('/api/v1/system/status');
  }

  async listPeers(): Promise<PeerNode[]> {
    return this.request<PeerNode[]>('/api/v1/system/peers');
  }

  async connectPeer(req: ConnectPeerRequest): Promise<PeerNode> {
    return this.request<PeerNode>('/api/v1/system/peers', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  async getClockSync(): Promise<ClockSyncStatus> {
    return this.request<ClockSyncStatus>('/api/v1/system/clock-sync');
  }

  // ==========================================================================
  // Audio Devices
  // ==========================================================================

  async listDevices(kind?: 'all' | 'input' | 'output' | 'virtual'): Promise<AudioDevice[]> {
    const q = kind && kind !== 'all' ? `?kind=${encodeURIComponent(kind)}` : '';
    return this.request<AudioDevice[]>(`/api/v1/devices${q}`);
  }

  async getDevice(deviceId: string): Promise<AudioDevice> {
    return this.request<AudioDevice>(`/api/v1/devices/${encodeURIComponent(deviceId)}`);
  }

  async updateDevice(deviceId: string, req: UpdateDeviceRequest): Promise<AudioDevice> {
    return this.request<AudioDevice>(`/api/v1/devices/${encodeURIComponent(deviceId)}`, {
      method: 'PATCH',
      body: JSON.stringify(req),
    });
  }

  async createVirtualDevice(req: CreateVirtualDeviceRequest): Promise<AudioDevice> {
    return this.request<AudioDevice>('/api/v1/devices/virtual', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  async deleteVirtualDevice(deviceId: string): Promise<{ deleted: boolean }> {
    return this.request<{ deleted: boolean }>(`/api/v1/devices/virtual/${encodeURIComponent(deviceId)}`, {
      method: 'DELETE',
    });
  }

  // ==========================================================================
  // Matrix Routes
  // ==========================================================================

  async listRoutes(): Promise<MatrixRoute[]> {
    return this.request<MatrixRoute[]>('/api/v1/routes');
  }

  async createRoute(req: CreateRouteRequest): Promise<MatrixRoute> {
    return this.request<MatrixRoute>('/api/v1/routes', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  async getRoute(routeId: string): Promise<MatrixRoute> {
    return this.request<MatrixRoute>(`/api/v1/routes/${encodeURIComponent(routeId)}`);
  }

  async patchRoute(routeId: string, req: PatchRouteRequest): Promise<MatrixRoute> {
    return this.request<MatrixRoute>(`/api/v1/routes/${encodeURIComponent(routeId)}`, {
      method: 'PATCH',
      body: JSON.stringify(req),
    });
  }

  async deleteRoute(routeId: string): Promise<{ deleted: boolean }> {
    return this.request<{ deleted: boolean }>(`/api/v1/routes/${encodeURIComponent(routeId)}`, {
      method: 'DELETE',
    });
  }

  async updateCrosspoint(routeId: string, req: CrosspointGainRequest): Promise<MatrixRoute> {
    return this.request<MatrixRoute>(`/api/v1/routes/${encodeURIComponent(routeId)}/crosspoint`, {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  // ==========================================================================
  // Room Orchestration
  // ==========================================================================

  async listRooms(): Promise<RoomInfo[]> {
    return this.request<RoomInfo[]>('/api/v1/rooms');
  }

  async createRoom(req: CreateRoomRequest): Promise<RoomInfo> {
    return this.request<RoomInfo>('/api/v1/rooms', {
      method: 'POST',
      body: JSON.stringify(req),
    });
  }

  async getRoom(roomId: string): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}`);
  }

  async deleteRoom(roomId: string): Promise<{ deleted: boolean }> {
    return this.request<{ deleted: boolean }>(`/api/v1/rooms/${encodeURIComponent(roomId)}`, {
      method: 'DELETE',
    });
  }

  async joinRoom(roomId: string): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}/join`, {
      method: 'POST',
    });
  }

  async leaveRoom(roomId: string): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}/leave`, {
      method: 'POST',
    });
  }

  async updateRoomBindings(roomId: string, req: UpdateRoomBindingsRequest): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}/bindings`, {
      method: 'PUT',
      body: JSON.stringify(req),
    });
  }

  async addRoomInput(roomId: string, devId: string, gain?: number): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}/inputs`, {
      method: 'POST',
      body: JSON.stringify({ device_id: devId, gain }),
    });
  }

  async removeRoomInput(roomId: string, devId: string): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}/inputs/${encodeURIComponent(devId)}`, {
      method: 'DELETE',
    });
  }

  async addRoomOutput(roomId: string, devId: string, gain?: number): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}/outputs`, {
      method: 'POST',
      body: JSON.stringify({ device_id: devId, gain }),
    });
  }

  async removeRoomOutput(roomId: string, devId: string): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}/outputs/${encodeURIComponent(devId)}`, {
      method: 'DELETE',
    });
  }

  async setRoomSync(roomId: string, synced: boolean): Promise<RoomInfo> {
    return this.request<RoomInfo>(`/api/v1/rooms/${encodeURIComponent(roomId)}/sync`, {
      method: 'POST',
      body: JSON.stringify({ synced }),
    });
  }

  // ==========================================================================
  // Telemetry Snapshot
  // ==========================================================================

  async getStreamStats(): Promise<Record<string, StreamStats>> {
    return this.request<Record<string, StreamStats>>('/api/v1/telemetry/stream-stats');
  }

  // ==========================================================================
  // Cluster Observability & Remote Device Management (ADR 0010 & ADR 0011)
  // ==========================================================================

  async getNetworkHealth(): Promise<NetworkHealthResponse> {
    return this.request<NetworkHealthResponse>('/api/v1/network/health');
  }

  async getClusterNodes(): Promise<ClusterNodesResponse> {
    return this.request<ClusterNodesResponse>('/api/v1/cluster/nodes');
  }

  async getClusterDevices(direction?: string): Promise<ClusterDevicesResponse> {
    const query = direction && direction !== 'all' ? `?direction=${encodeURIComponent(direction)}` : '';
    return this.request<ClusterDevicesResponse>(`/api/v1/cluster/devices${query}`);
  }

  async patchClusterDevice(
    nodeId: string,
    deviceId: string,
    req: DeviceMutationRequest
  ): Promise<DeviceMutationResponse> {
    return this.request<DeviceMutationResponse>(
      `/api/v1/cluster/nodes/${encodeURIComponent(nodeId)}/devices/${encodeURIComponent(deviceId)}`,
      {
        method: 'PATCH',
        body: JSON.stringify(req),
      }
    );
  }
}

export const apiClient = new ApiClient();
