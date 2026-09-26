import type { MatrixRoute, CrosspointGainRequest, CreateRouteRequest } from '$lib/api/types';
import { apiClient } from '$lib/api/client';
import { linearToDb, dbToLinear } from '$lib/api/types';

export interface CrosspointCell {
  routeId: string;
  inNodeId: string;
  inDeviceId: string;
  inChannel: number;
  outNodeId: string;
  outDeviceId: string;
  outChannel: number;
  gainLinear: number;
  gainDb: number;
  muted: boolean;
  isSlewing: boolean;
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

  // Active crosspoint lookup: key = `${outKey}->${inKey}`
  crosspointMap = $state<Record<string, CrosspointCell>>({});

  // Active slew timers to display 20ms transition feedback
  activeSlews = $state<Record<string, boolean>>({});

  // Active popover fader target
  activeFaderCell = $state<CrosspointCell | null>(null);
  activeFaderCoords = $state<{ x: number; y: number } | null>(null);

  public static makePortKey(nodeId: string, deviceId: string, ch: number): string {
    return `${nodeId}:${deviceId}:${ch}`;
  }

  public static makeCrosspointKey(outPortKey: string, inPortKey: string): string {
    return `${outPortKey}->${inPortKey}`;
  }

  async loadRoutes() {
    this.isLoading = true;
    this.error = null;
    try {
      const fetched = await apiClient.listRoutes();
      this.routes = fetched;
      this.rebuildCrosspointMap();
    } catch (err: any) {
      this.error = err.message || 'Failed to load matrix routes';
    } finally {
      this.isLoading = false;
    }
  }

  private rebuildCrosspointMap() {
    const newMap: Record<string, CrosspointCell> = {};

    for (const route of this.routes) {
      const srcNode = route.source.node_id || 'local';
      const srcDev = route.source.device_id;
      const dstNode = route.destination.node_id || 'local';
      const dstDev = route.destination.device_id;

      const srcChannels = route.source.channels || [];
      const dstChannels = route.destination.channels || [];
      const coeffs = route.coefficients || [];
      const inCount = route.in_channels || srcChannels.length || 1;
      const outCount = route.out_channels || dstChannels.length || 1;

      for (let outIdx = 0; outIdx < outCount; outIdx++) {
        const outCh = dstChannels[outIdx] ?? outIdx;
        const outKey = MatrixStore.makePortKey(dstNode, dstDev, outCh);

        for (let inIdx = 0; inIdx < inCount; inIdx++) {
          const inCh = srcChannels[inIdx] ?? inIdx;
          const inKey = MatrixStore.makePortKey(srcNode, srcDev, inCh);
          const cpKey = MatrixStore.makeCrosspointKey(outKey, inKey);

          const coeffIdx = outIdx * inCount + inIdx;
          const gainLinear = coeffs[coeffIdx] !== undefined ? coeffs[coeffIdx] : 0.0;

          if (gainLinear > 0.00001) {
            newMap[cpKey] = {
              routeId: route.id,
              inNodeId: srcNode,
              inDeviceId: srcDev,
              inChannel: inCh,
              outNodeId: dstNode,
              outDeviceId: dstDev,
              outChannel: outCh,
              gainLinear,
              gainDb: linearToDb(gainLinear),
              muted: !!route.muted,
              isSlewing: route.state === 'slewing' || !!this.activeSlews[cpKey],
            };
          }
        }
      }
    }

    this.crosspointMap = newMap;
  }

  // 1-Click Connect (at unity 0.0 dB) or Disconnect
  async toggleCrosspoint(
    inPort: { nodeId: string; deviceId: string; channel: number },
    outPort: { nodeId: string; deviceId: string; channel: number }
  ) {
    const inKey = MatrixStore.makePortKey(inPort.nodeId, inPort.deviceId, inPort.channel);
    const outKey = MatrixStore.makePortKey(outPort.nodeId, outPort.deviceId, outPort.channel);
    const cpKey = MatrixStore.makeCrosspointKey(outKey, inKey);

    const existing = this.crosspointMap[cpKey];

    if (existing) {
      // Disconnect: set gain to 0.0 or delete route if single crosspoint
      await this.setGain(existing.routeId, existing.outChannel, existing.inChannel, 0.0, 20.0);
    } else {
      // Connect at unity 0.0 dB (linear 1.0)
      // Check if route exists between these devices
      const existingRoute = this.routes.find(
        (r) =>
          r.source.node_id === inPort.nodeId &&
          r.source.device_id === inPort.deviceId &&
          r.destination.node_id === outPort.nodeId &&
          r.destination.device_id === outPort.deviceId
      );

      if (existingRoute) {
        await this.setGain(existingRoute.id, outPort.channel, inPort.channel, 1.0, 20.0);
      } else {
        // Create new route
        const newRouteReq: CreateRouteRequest = {
          name: `${inPort.deviceId}:${inPort.channel} -> ${outPort.deviceId}:${outPort.channel}`,
          source: {
            node_id: inPort.nodeId,
            device_id: inPort.deviceId,
            channels: [inPort.channel],
          },
          destination: {
            node_id: outPort.nodeId,
            device_id: outPort.deviceId,
            channels: [outPort.channel],
          },
          coefficients: [1.0],
          slew_ms: 20.0,
        };

        try {
          const created = await apiClient.createRoute(newRouteReq);
          this.routes = [...this.routes, created];
          this.rebuildCrosspointMap();
        } catch (err: any) {
          this.error = err.message || 'Failed to create route';
        }
      }
    }
  }

  async setGain(
    routeId: string,
    outChannel: number,
    inChannel: number,
    gainLinear: number,
    slewMs: number = 20.0
  ) {
    // Slew visual feedback
    const route = this.routes.find((r) => r.id === routeId);
    let cpKey = '';
    if (route) {
      const inKey = MatrixStore.makePortKey(route.source.node_id, route.source.device_id, inChannel);
      const outKey = MatrixStore.makePortKey(route.destination.node_id, route.destination.device_id, outChannel);
      cpKey = MatrixStore.makeCrosspointKey(outKey, inKey);
      this.activeSlews[cpKey] = true;
    }

    try {
      const updatedRoute = await apiClient.updateCrosspoint(routeId, {
        out_channel: outChannel,
        in_channel: inChannel,
        gain: gainLinear,
        slew_ms: slewMs,
      });

      // Update local route
      const idx = this.routes.findIndex((r) => r.id === routeId);
      if (idx !== -1) {
        this.routes[idx] = updatedRoute;
      } else {
        this.routes.push(updatedRoute);
      }

      this.rebuildCrosspointMap();

      // Clear slew indicator after slewMs duration
      setTimeout(() => {
        if (cpKey) {
          delete this.activeSlews[cpKey];
          if (this.crosspointMap[cpKey]) {
            this.crosspointMap[cpKey].isSlewing = false;
          }
        }
      }, slewMs + 50);
    } catch (err: any) {
      this.error = err.message || 'Failed to update crosspoint';
      if (cpKey) delete this.activeSlews[cpKey];
    }
  }

  async setMute(routeId: string, muted: boolean) {
    try {
      const updatedRoute = await apiClient.patchRoute(routeId, { muted });
      const idx = this.routes.findIndex((r) => r.id === routeId);
      if (idx !== -1) {
        this.routes[idx] = updatedRoute;
      }
      this.rebuildCrosspointMap();
    } catch (err: any) {
      this.error = err.message || 'Failed to toggle mute';
    }
  }

  async deleteRoute(routeId: string) {
    try {
      await apiClient.deleteRoute(routeId);
      this.routes = this.routes.filter((r) => r.id !== routeId);
      this.rebuildCrosspointMap();
    } catch (err: any) {
      this.error = err.message || 'Failed to delete route';
    }
  }

  openFaderPopover(cell: CrosspointCell, coords: { x: number; y: number }) {
    this.activeFaderCell = cell;
    this.activeFaderCoords = coords;
  }

  closeFaderPopover() {
    this.activeFaderCell = null;
    this.activeFaderCoords = null;
  }
}

export const matrixStore = new MatrixStore();
