import type {
  AudioDevice,
  MatrixRoute,
  CrosspointGainRequest,
  CreateRouteRequest,
  PatchRouteRequest,
} from '$lib/api/types';
import { apiClient } from '$lib/api/client';
import { linearToDb, dbToLinear } from '$lib/api/types';

export interface CrosspointCell {
  routeId: string;
  sourceDeviceId: string;
  destinationDeviceId: string;
  sourceNodeId?: string;
  destinationNodeId?: string;
  gainLinear: number;
  gainDb: number;
  muted: boolean;
  isSlewing: boolean;
  state: 'active' | 'slewing' | 'paused' | 'error';
  route: MatrixRoute;
  // Backward compatibility / channel details:
  inDeviceId?: string;
  outDeviceId?: string;
  inChannel?: number;
  outChannel?: number;
  inNodeId?: string;
  outNodeId?: string;
}

export interface MatrixPort {
  id: string; // "nodeId:deviceId:ch"
  nodeId: string;
  deviceId: string;
  deviceName: string;
  channel: number;
  channelLabel: string;
  isVirtual: boolean;
}

class MatrixStore {
  routes = $state<MatrixRoute[]>([]);
  isLoading = $state<boolean>(false);
  error = $state<string | null>(null);
  lastError = $state<string | null>(null);

  // Active crosspoint lookup: key = `${destinationDeviceId}->${sourceDeviceId}`
  crosspointMap = $state<Record<string, CrosspointCell>>({});

  // Active slew timers to display 20ms transition feedback
  activeSlews = $state<Record<string, boolean>>({});

  // Active popover fader target
  activeFaderCell = $state<CrosspointCell | null>(null);
  activeFaderCoords = $state<{ x: number; y: number } | null>(null);
  activeFaderSourceDevice = $state<AudioDevice | null>(null);
  activeFaderDestinationDevice = $state<AudioDevice | null>(null);

  // Route Inspector drawer target: non-null route id means the drawer is open.
  inspectorRouteId = $state<string | null>(null);

  openInspector(routeId: string) {
    this.inspectorRouteId = routeId;
    this.closeFaderPopover();
  }

  closeInspector() {
    this.inspectorRouteId = null;
  }

  clearError() {
    this.lastError = null;
    this.error = null;
  }

  public static makePortKey(nodeId: string, deviceId: string, ch: number): string {
    return `${nodeId}:${deviceId}:${ch}`;
  }

  public static makeCrosspointKey(destinationDeviceId: string, sourceDeviceId: string): string {
    return `${destinationDeviceId}->${sourceDeviceId}`;
  }

  getCrosspoint(destinationDeviceId: string, sourceDeviceId: string): CrosspointCell | undefined {
    return this.crosspointMap[MatrixStore.makeCrosspointKey(destinationDeviceId, sourceDeviceId)];
  }

  async loadRoutes() {
    this.isLoading = true;
    this.clearError();
    try {
      const fetched = await apiClient.listRoutes();
      this.routes = fetched;
      this.rebuildCrosspointMap();
    } catch (err: any) {
      const msg = err?.message || 'Failed to load matrix routes';
      this.lastError = msg;
      this.error = msg;
    } finally {
      this.isLoading = false;
    }
  }

  private rebuildCrosspointMap() {
    const newMap: Record<string, CrosspointCell> = {};

    for (const route of this.routes) {
      const srcDev = route.source.device_id;
      const dstDev = route.destination.device_id;
      if (!srcDev || !dstDev) continue;

      const cpKey = MatrixStore.makeCrosspointKey(dstDev, srcDev);
      const nonZero = route.coefficients?.filter((c) => c > 0.00001) ?? [];
      const gainLinear = nonZero.length > 0 ? Math.max(...nonZero) : (route.coefficients?.[0] ?? 1.0);

      newMap[cpKey] = {
        routeId: route.id,
        sourceDeviceId: srcDev,
        destinationDeviceId: dstDev,
        sourceNodeId: route.source.node_id || 'local',
        destinationNodeId: route.destination.node_id || 'local',
        gainLinear,
        gainDb: linearToDb(gainLinear),
        muted: !!route.muted,
        isSlewing: route.state === 'slewing' || !!this.activeSlews[cpKey],
        state: route.state,
        route,
        // Legacy compatibility:
        inDeviceId: srcDev,
        outDeviceId: dstDev,
        inChannel: route.source.channels?.[0] ?? 0,
        outChannel: route.destination.channels?.[0] ?? 0,
        inNodeId: route.source.node_id || 'local',
        outNodeId: route.destination.node_id || 'local',
      };
    }

    this.crosspointMap = newMap;
  }

  // 1-Click Connect (at unity 0.0 dB) or Disconnect between Source Device and Destination Device
  async toggleCrosspoint(
    sourceDevice: AudioDevice | { id?: string; deviceId?: string; name?: string; max_input?: number },
    destDevice: AudioDevice | { id?: string; deviceId?: string; name?: string; max_output?: number }
  ) {
    const srcId = 'id' in sourceDevice && sourceDevice.id ? sourceDevice.id : (sourceDevice as any).deviceId;
    const dstId = 'id' in destDevice && destDevice.id ? destDevice.id : (destDevice as any).deviceId;
    const srcName = sourceDevice.name || srcId;
    const dstName = destDevice.name || dstId;
    const srcMaxIn = 'max_input' in sourceDevice && typeof sourceDevice.max_input === 'number' ? sourceDevice.max_input : 2;
    const dstMaxOut = 'max_output' in destDevice && typeof destDevice.max_output === 'number' ? destDevice.max_output : 2;

    // Validate direction capabilities
    if (srcMaxIn <= 0) {
      const msg = `Cannot route from ${srcName}: device has no input channels; outputs cannot be used as sources`;
      this.lastError = msg;
      this.error = msg;
      return;
    }
    if (dstMaxOut <= 0) {
      const msg = `Cannot route to ${dstName}: device has no output channels; inputs cannot be used as destinations`;
      this.lastError = msg;
      this.error = msg;
      return;
    }

    const cpKey = MatrixStore.makeCrosspointKey(dstId, srcId);
    const existing = this.crosspointMap[cpKey];

    if (existing) {
      // Disconnect: delete existing route
      await this.deleteRoute(existing.routeId);
    } else {
      // Connect at unity gain 0.0 dB (linear 1.0)
      const inCount = Math.max(1, srcMaxIn);
      const outCount = Math.max(1, dstMaxOut);
      const inChannels = Array.from({ length: inCount }, (_, i) => i);
      const outChannels = Array.from({ length: outCount }, (_, i) => i);

      // Identity 1:1 pass-through coefficients
      const coeffs = new Array(outCount * inCount).fill(0);
      const minCh = Math.min(inCount, outCount);
      for (let i = 0; i < minCh; i++) {
        coeffs[i * inCount + i] = 1.0;
      }
      if (inCount === 1 && outCount === 2) {
        // Mono source to stereo destination
        coeffs[0] = 1.0;
        coeffs[1] = 1.0;
      }

      const newRouteReq: CreateRouteRequest = {
        name: `${srcName} -> ${dstName}`,
        source: {
          node_id: 'local',
          device_id: srcId,
          channels: inChannels,
        },
        destination: {
          node_id: 'local',
          device_id: dstId,
          channels: outChannels,
        },
        coefficients: coeffs,
        slew_ms: 20.0,
      };

      try {
        this.clearError();
        const created = await apiClient.createRoute(newRouteReq);
        this.routes = [...this.routes, created];
        this.rebuildCrosspointMap();
      } catch (err: any) {
        const msg = err?.message || 'Failed to create route';
        this.lastError = msg;
        this.error = msg;
      }
    }
  }

  async setGain(
    routeId: string,
    arg2: number,
    arg3?: number,
    arg4?: number,
    arg5?: number
  ) {
    let gainLinear: number;
    let slewMs = 20.0;
    let outChannel: number | null = null;
    let inChannel: number | null = null;

    if (typeof arg4 === 'number') {
      // Legacy channel-level: setGain(routeId, outCh, inCh, gainLinear, slewMs)
      outChannel = arg2;
      inChannel = arg3 ?? 0;
      gainLinear = arg4;
      slewMs = arg5 ?? 20.0;
    } else {
      // Device/Route-level: setGain(routeId, gainLinear, slewMs)
      gainLinear = arg2;
      slewMs = arg3 ?? 20.0;
    }

    const route = this.routes.find((r) => r.id === routeId);
    let cpKey = '';
    if (route) {
      cpKey = MatrixStore.makeCrosspointKey(route.destination.device_id, route.source.device_id);
      this.activeSlews[cpKey] = true;
    }

    try {
      this.clearError();
      let updatedRoute: MatrixRoute;

      if (outChannel !== null && inChannel !== null) {
        updatedRoute = await apiClient.updateCrosspoint(routeId, {
          out_channel: outChannel,
          in_channel: inChannel,
          gain: gainLinear,
          slew_ms: slewMs,
        });
      } else if (route) {
        const inCount = route.in_channels || route.source.channels?.length || 1;
        const outCount = route.out_channels || route.destination.channels?.length || 1;
        const currentCoeffs = route.coefficients || [];
        const nonZero = currentCoeffs.filter((c) => c > 0.0001);
        const currentMax = nonZero.length > 0 ? Math.max(...nonZero) : 1.0;

        let newCoeffs: number[];
        if (currentCoeffs.length === outCount * inCount && currentMax > 0.0001) {
          const scale = gainLinear / currentMax;
          newCoeffs = currentCoeffs.map((c) => Math.max(0, Math.min(2.0, c * scale)));
        } else {
          newCoeffs = new Array(outCount * inCount).fill(0);
          const minCh = Math.min(inCount, outCount);
          for (let i = 0; i < minCh; i++) {
            newCoeffs[i * inCount + i] = gainLinear;
          }
          if (inCount === 1 && outCount === 2) {
            newCoeffs[0] = gainLinear;
            newCoeffs[1] = gainLinear;
          }
        }

        updatedRoute = await apiClient.patchRoute(routeId, {
          coefficients: newCoeffs,
          slew_ms: slewMs,
        });
      } else {
        return;
      }

      const idx = this.routes.findIndex((r) => r.id === routeId);
      if (idx !== -1) {
        this.routes[idx] = updatedRoute;
      }
      this.rebuildCrosspointMap();

      setTimeout(() => {
        if (cpKey) {
          delete this.activeSlews[cpKey];
          if (this.crosspointMap[cpKey]) {
            this.crosspointMap[cpKey].isSlewing = false;
          }
        }
      }, slewMs + 50);
    } catch (err: any) {
      const msg = err?.message || 'Failed to update crosspoint gain';
      this.lastError = msg;
      this.error = msg;
      if (cpKey) delete this.activeSlews[cpKey];
    }
  }

  async setMute(routeId: string, muted: boolean) {
    try {
      this.clearError();
      const updatedRoute = await apiClient.patchRoute(routeId, { muted });
      const idx = this.routes.findIndex((r) => r.id === routeId);
      if (idx !== -1) {
        this.routes[idx] = updatedRoute;
      }
      this.rebuildCrosspointMap();
    } catch (err: any) {
      const msg = err?.message || 'Failed to toggle mute';
      this.lastError = msg;
      this.error = msg;
    }
  }

  async deleteRoute(routeId: string) {
    try {
      this.clearError();
      await apiClient.deleteRoute(routeId);
      this.routes = this.routes.filter((r) => r.id !== routeId);
      this.rebuildCrosspointMap();
    } catch (err: any) {
      const msg = err?.message || 'Failed to delete route';
      this.lastError = msg;
      this.error = msg;
    }
  }

  openFaderPopover(
    cell: CrosspointCell,
    coords: { x: number; y: number },
    sourceDevice?: AudioDevice,
    destDevice?: AudioDevice
  ) {
    this.activeFaderCell = cell;
    this.activeFaderCoords = coords;
    this.activeFaderSourceDevice = sourceDevice || null;
    this.activeFaderDestinationDevice = destDevice || null;
  }

  closeFaderPopover() {
    this.activeFaderCell = null;
    this.activeFaderCoords = null;
    this.activeFaderSourceDevice = null;
    this.activeFaderDestinationDevice = null;
  }
}

export const matrixStore = new MatrixStore();
