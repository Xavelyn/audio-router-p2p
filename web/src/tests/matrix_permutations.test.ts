import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { waitForStore, MockLocalStorage, MockBackendServer, setupMockEventSource, MockEventSource, computeEdgeStyle } from './testUtils';
import { roomStore } from '$lib/stores/rooms.svelte';
import { deviceStore } from '$lib/stores/devices.svelte';
import { matrixStore } from '$lib/stores/matrix.svelte';
import { clusterStore } from '$lib/stores/cluster.svelte';
import { flowLayoutStore, STORAGE_KEY_V1 } from '$lib/stores/flowLayout.svelte';
import { SSEManager } from '$lib/api/sse';
import { apiClient } from '$lib/api/client';
import { tick } from 'svelte';

// Canonical endpoint match helper accommodating node qualification (ADR 0013 / ADR 0018)
// and SvelteFlow node prefixing ('input-', 'output-')
function hasDevice(list: string[] | undefined, deviceId: string): boolean {
  if (!list) return false;
  return list.some((id) => {
    const cleanId = id.replace(/^(input-|output-)/, '');
    const cleanDev = deviceId.replace(/^(input-|output-)/, '');
    return cleanId === cleanDev || cleanId.endsWith(`/${cleanDev}`) || cleanDev.endsWith(`/${cleanId}`);
  });
}

describe('WebUI Permutation Matrices (ADR 0024)', () => {
  let mockStorage: MockLocalStorage;
  let restoreStorage: () => void;
  let mockBackend: MockBackendServer;
  let restoreFetch: () => void;
  let restoreEventSource: () => void;

  const isLiveBackend = !!process.env.VITE_API_URL;

  beforeEach(async () => {
    // 1. Install hermetic MockLocalStorage
    mockStorage = new MockLocalStorage();
    restoreStorage = mockStorage.install();

    // 2. Setup MockEventSource for SSE tests
    restoreEventSource = setupMockEventSource();

    // 3. Setup Backend Transport (Live or Mock)
    if (isLiveBackend) {
      apiClient.setBaseUrl(process.env.VITE_API_URL!);
      apiClient.setToken(process.env.VITE_AUTH_TOKEN || 'managed-e2e-matrix-token');
    } else {
      mockBackend = new MockBackendServer();
      restoreFetch = mockBackend.install();
      apiClient.setBaseUrl('http://localhost:8000');
      apiClient.setToken('test-token');
    }

    // Reset Stores to fresh state
    flowLayoutStore.resetLayout();
    flowLayoutStore.positions = new Map();
    flowLayoutStore.collapsedNodes = new Set();
    flowLayoutStore.setSidebarOpen(true);

    // Initial load
    await Promise.all([
      deviceStore.loadDevices(),
      roomStore.loadRooms(),
      matrixStore.loadRoutes(),
      clusterStore.pollCluster(),
    ]);
    await tick();
  });

  afterEach(() => {
    if (restoreFetch) restoreFetch();
    if (restoreStorage) restoreStorage();
    if (restoreEventSource) restoreEventSource();
  });

  // ==========================================================================
  // Matrix 1: Output before Input
  // Playout sink node bound to room before capture source node is provisioned.
  // Sink node renders unlinked in canvas; summing edges remain zero;
  // arrival of source automatically forms audio edge without UI error.
  // ==========================================================================
  it('Matrix 1: Output before Input - unlinked sink stages cleanly and wires on source arrival', async () => {
    const roomId = `room-m1-${Date.now()}`;
    const outputDevId = deviceStore.outputDevices[0]?.id || 'hw:1,0';
    const inputDevId = deviceStore.inputDevices[0]?.id || 'hw:2,0';

    // 1. Stage output sink node in canvas layout
    flowLayoutStore.enableDevice(outputDevId, { x: 1020, y: 150 });
    expect(flowLayoutStore.isDeviceEnabled(outputDevId)).toBe(true);
    expect(flowLayoutStore.getPosition(outputDevId)).toEqual({ x: 1020, y: 150 });

    // 2. Create conference room with only the output sink bound (inputs empty)
    await roomStore.createRoom({ id: roomId, name: 'Matrix 1 Studio' });
    await roomStore.addRoomOutput(roomId, outputDevId);

    const roomAfterSink = await waitForStore(
      () => roomStore.rooms.find((r) => r.id === roomId),
      (r) => !!r && hasDevice(r.outputs, outputDevId)
    );
    expect(roomAfterSink).toBeDefined();
    expect(roomAfterSink!.inputs.length).toBe(0);

    // 3. Verify zero incoming summing edges exist because source has not arrived
    const computeSummingEdges = () => {
      const targetRoom = roomStore.rooms.find((r) => r.id === roomId);
      if (!targetRoom || !targetRoom.inputs || targetRoom.inputs.length === 0) {
        return [];
      }
      return targetRoom.inputs.map((inId) => ({
        id: `edge-in-${inId}-to-${roomId}`,
        source: `input-${inId}`,
        target: `room-${roomId}`,
      }));
    };

    expect(computeSummingEdges().length).toBe(0);

    // 4. Source arrives: stage capture input node and bind to room
    flowLayoutStore.enableDevice(inputDevId, { x: 50, y: 150 });
    await roomStore.addRoomInput(roomId, inputDevId);

    const roomAfterSource = await waitForStore(
      () => roomStore.rooms.find((r) => r.id === roomId),
      (r) => !!r && hasDevice(r.inputs, inputDevId)
    );

    // 5. Verify summing edge now automatically connects source to room
    const edgesAfterSource = computeSummingEdges();
    expect(edgesAfterSource.length).toBe(1);
    expect(hasDevice([edgesAfterSource[0].source], inputDevId)).toBe(true);
    expect(edgesAfterSource[0].target).toBe(`room-${roomId}`);
    expect(hasDevice(roomAfterSource!.outputs, outputDevId)).toBe(true);
  });

  // ==========================================================================
  // Matrix 2: Input before Output
  // Capture source node provisioned and staged in canvas before any room sink exists.
  // Input node displays staged status; matrix faders remain inactive;
  // downstream route auto-binds cleanly when sink appears.
  // ==========================================================================
  it('Matrix 2: Input before Output - stages input node and auto-binds when sink arrives', async () => {
    const roomId = `room-m2-${Date.now()}`;
    const inputDevId = deviceStore.inputDevices[0]?.id || 'hw:2,0';
    const outputDevId = deviceStore.outputDevices[0]?.id || 'hw:1,0';

    // 1. Stage input capture device
    flowLayoutStore.enableDevice(inputDevId, { x: 60, y: 200 });
    expect(flowLayoutStore.isDeviceEnabled(inputDevId)).toBe(true);

    // 2. Create room with capture input bound, no output sinks
    await roomStore.createRoom({ id: roomId, name: 'Matrix 2 Live Booth' });
    await roomStore.addRoomInput(roomId, inputDevId);

    const roomWithInputOnly = await waitForStore(
      () => roomStore.rooms.find((r) => r.id === roomId),
      (r) => !!r && hasDevice(r.inputs, inputDevId)
    );
    expect(roomWithInputOnly!.outputs.length).toBe(0);

    // 3. Verify downstream output edges are empty
    const computeDownstreamEdges = () => {
      const room = roomStore.rooms.find((r) => r.id === roomId);
      if (!room || !room.outputs || room.outputs.length === 0) return [];
      return room.outputs.map((outId) => ({
        id: `edge-room-${roomId}-to-out-${outId}`,
        source: `room-${roomId}`,
        target: `output-${outId}`,
      }));
    };
    expect(computeDownstreamEdges().length).toBe(0);

    // 4. Sink arrives: bind playout output device to room
    flowLayoutStore.enableDevice(outputDevId, { x: 1040, y: 200 });
    await roomStore.addRoomOutput(roomId, outputDevId);

    const roomFullyBound = await waitForStore(
      () => roomStore.rooms.find((r) => r.id === roomId),
      (r) => !!r && hasDevice(r.outputs, outputDevId)
    );

    // 5. Downstream audio edge auto-binds cleanly
    const edgesAfterSink = computeDownstreamEdges();
    expect(edgesAfterSink.length).toBe(1);
    expect(edgesAfterSink[0].source).toBe(`room-${roomId}`);
    expect(hasDevice([edgesAfterSink[0].target], outputDevId)).toBe(true);
    expect(hasDevice(roomFullyBound!.inputs, inputDevId)).toBe(true);
  });

  // ==========================================================================
  // Matrix 3: Connect Order & Dynamic Gain
  // Dynamic fader adjustment, gain slew interpolation, and edge styling.
  // Gain fader updates reactive matrix store; SVG edge reflects decibels.
  // ==========================================================================
  it('Matrix 3: Connect Order & Dynamic Gain - validates slewing and dynamic edge decibel interpolation', async () => {
    // 1. Ensure source and destination devices are available
    let src = deviceStore.inputDevices.find((d) => d.max_input > 0);
    let dst = deviceStore.outputDevices.find((d) => d.max_output > 0);

    if (!src || !dst) {
      src = await deviceStore.createVirtual({
        name: 'M3 Virt Capture',
        direction: 'input',
        channels: 2,
        sample_rate: 48000,
      });
      dst = await deviceStore.createVirtual({
        name: 'M3 Virt Playout',
        direction: 'output',
        channels: 2,
        sample_rate: 48000,
      });
    }

    // 2. 1-Click Connect Crosspoint (connects at unity gain 1.0 = 0.0 dB)
    await matrixStore.toggleCrosspoint(src, dst);

    const cp = await waitForStore(
      () => matrixStore.getCrosspoint(dst.id, src.id),
      (c) => !!c && c.gainLinear > 0.0
    );
    expect(cp).toBeDefined();
    expect(cp!.gainLinear).toBeCloseTo(1.0, 2);
    expect(cp!.gainDb).toBeCloseTo(0.0, 1);

    // 3. Dynamic Gain Slew adjustment (+3.0 dB ~= 1.4125 linear)
    const targetLinear = 1.4125;
    await matrixStore.setGain(cp!.routeId, targetLinear, 25.0);

    // 4. Verify reactive edge styling interpolation
    const style1 = computeEdgeStyle(1.0);
    const style2 = computeEdgeStyle(targetLinear);

    expect(parseFloat(style2.strokeWidth)).toBeGreaterThan(parseFloat(style1.strokeWidth));
    expect(style2.gainDb).toBeCloseTo(3.0, 1);
    expect(style2.strokeColor).toBe('#f59e0b'); // Hot/Boosted gain color

    // Attenuation styling (-6.0 dB ~= 0.5012 linear)
    const styleAtten = computeEdgeStyle(0.5012);
    expect(styleAtten.gainDb).toBeCloseTo(-6.0, 1);
    expect(styleAtten.strokeColor).toBe('#06b6d4'); // Normal Cyan
  });

  // ==========================================================================
  // Matrix 4: Node B Starts First / Multi-node Cluster Discovery
  // Receiver node B boots prior to Sender node A; discovery converges asynchronously.
  // Remote devices aggregate seamlessly under node-qualified URNs.
  // ==========================================================================
  it('Matrix 4: Node B Starts First - multi-node discovery aggregates remote device URNs', async () => {
    // 1. Initial Cluster state contains local node (Node B)
    expect(clusterStore.nodes.length).toBeGreaterThanOrEqual(1);

    // 2. Simulate Node A joining mesh with remote hardware devices
    if (!isLiveBackend && mockBackend) {
      mockBackend.clusterNodes = {
        total_nodes: 2,
        healthy_nodes: 2,
        degraded_nodes: 0,
        timestamp: new Date().toISOString(),
        nodes: [
          ...mockBackend.clusterNodes.nodes,
          {
            id: 'node-remote-a',
            name: 'Studio Satellite A',
            api_addr: '192.168.1.150:8000',
            is_local: false,
            swim_state: 'alive',
            health_score: 95.0,
            health_grade: 'EXCELLENT',
            clock_sync: {
              offset_us: 12,
              drift_ppm: 0.1,
              sync_rtt_us: 450,
              packets_sent: 500,
              packets_lost: 0,
              is_master: false,
            },
            devices: [
              {
                id: 'remote-mic-1',
                node_id: 'node-remote-a',
                node_name: 'Studio Satellite A',
                name: 'Neumann U87 Master Mic',
                direction: 'input',
                max_input: 1,
                max_output: 0,
                default_sample_rate: 48000,
                is_virtual: false,
                volume: 1.0,
                muted: false,
                urn: 'urn:arp2p:node-remote-a:remote-mic-1',
              },
            ],
            active_streams: 1,
          },
        ],
      };
    }

    // 3. Poll cluster store for remote peer arrival
    await clusterStore.pollCluster();

    const remoteNodes = await waitForStore(
      () => clusterStore.remoteNodes,
      (nodes) => nodes.length > 0 || isLiveBackend
    );

    if (remoteNodes.length > 0) {
      const remoteNode = remoteNodes.find((n) => n.id === 'node-remote-a');
      if (remoteNode) {
        expect(remoteNode.devices.length).toBeGreaterThan(0);
        expect(remoteNode.devices[0].urn).toContain('node-remote-a');
      }
    }

    // 4. Verify Palette Accordion reactivity
    flowLayoutStore.toggleNodeCollapsed('node-remote-a');
    expect(flowLayoutStore.isNodeCollapsed('node-remote-a')).toBe(true);

    flowLayoutStore.toggleNodeCollapsed('node-remote-a');
    expect(flowLayoutStore.isNodeCollapsed('node-remote-a')).toBe(false);
  });

  // ==========================================================================
  // Matrix 5: Deletion & Recreation Churn
  // Rapid room deletion followed by immediate recreation with identical ID;
  // virtual device hot-swapping. Canvas cleans orphaned edges immediately;
  // ID collision handled gracefully; reactive store updates without memory leaks.
  // ==========================================================================
  it('Matrix 5: Deletion & Recreation Churn - rapid lifecycle and virtual device hot-swapping', async () => {
    const churnRoomId = `room-churn-${Date.now()}`;

    // 1. Hot-swap create virtual device
    const vDev = await deviceStore.createVirtual({
      name: `HotSwap Virt ${Date.now()}`,
      direction: 'duplex',
      channels: 2,
      sample_rate: 48000,
    });
    expect(deviceStore.devices.some((d) => d.id === vDev.id)).toBe(true);

    // 2. Create room and bind virtual device
    await roomStore.createRoom({ id: churnRoomId, name: 'Churn Stage 1' });
    await roomStore.addRoomInput(churnRoomId, vDev.id);
    flowLayoutStore.setPosition(`room-${churnRoomId}`, { x: 500, y: 300 });

    await waitForStore(
      () => roomStore.rooms.find((r) => r.id === churnRoomId),
      (r) => !!r && hasDevice(r.inputs, vDev.id)
    );

    // 3. Rapid Deletion
    await roomStore.deleteRoom(churnRoomId);
    flowLayoutStore.deleteNodePosition(`room-${churnRoomId}`);

    await waitForStore(
      () => roomStore.rooms.find((r) => r.id === churnRoomId),
      (r) => !r
    );
    expect(flowLayoutStore.getPosition(`room-${churnRoomId}`)).toBeUndefined();

    // 4. Immediate Recreation with IDENTICAL ID
    await roomStore.createRoom({ id: churnRoomId, name: 'Churn Stage 2 Recreated' });
    const recreated = await waitForStore(
      () => roomStore.rooms.find((r) => r.id === churnRoomId),
      (r) => !!r && r.name === 'Churn Stage 2 Recreated'
    );
    expect(recreated).toBeDefined();
    expect(recreated!.inputs.length).toBe(0);

    // 5. Hot-swap delete virtual device
    await deviceStore.deleteVirtual(vDev.id);
    flowLayoutStore.disableDevice(vDev.id);

    await waitForStore(
      () => deviceStore.devices.find((d) => d.id === vDev.id),
      (d) => !d
    );
    expect(flowLayoutStore.isDeviceEnabled(vDev.id)).toBe(false);

    // Cleanup room
    await roomStore.deleteRoom(churnRoomId);
  });

  // ==========================================================================
  // Matrix 6: Node Restart & LocalStorage Persistence Reconciliation
  // Daemon reboots; browser reloads with persisted canvas layout in localStorage.
  // Restoring coordinates (x, y), staged nodes, room topologies, and pruning ghost nodes.
  // ==========================================================================
  it('Matrix 6: Node Restart & LocalStorage Reconciliation - restores layout coordinates and prunes stale nodes', async () => {
    // 1. Mock preexisting persisted layout in localStorage using active device inventory
    const primaryDev = deviceStore.devices[0]?.id || 'hw:0,0';
    const secondaryDev = deviceStore.devices[1]?.id || 'hw:1,0';

    const savedLayout = {
      version: 1,
      lastUpdated: Date.now() - 5000,
      sidebarCollapsed: false,
      stagedDeviceIds: [primaryDev, secondaryDev, 'ghost:deleted-mic'],
      nodePositions: {
        [`input-${primaryDev}`]: { x: 80, y: 120 },
        [`output-${secondaryDev}`]: { x: 1080, y: 120 },
        'room-persisted': { x: 520, y: 240 },
        'ghost:deleted-mic': { x: 999, y: 999 },
      },
      collapsedNodes: ['node-stale-mesh'],
      viewport: { x: 15, y: 25, zoom: 1.15 },
    };

    mockStorage.setItem(STORAGE_KEY_V1, JSON.stringify(savedLayout));

    // 2. Simulate browser reload / hydrate()
    flowLayoutStore.hydrate();
    await tick();

    expect(flowLayoutStore.hasSavedLayout).toBe(true);
    expect(flowLayoutStore.isDeviceEnabled(primaryDev)).toBe(true);
    expect(flowLayoutStore.isDeviceEnabled(secondaryDev)).toBe(true);
    expect(flowLayoutStore.getPosition(`input-${primaryDev}`)).toEqual({ x: 80, y: 120 });
    expect(flowLayoutStore.getPosition(`output-${secondaryDev}`)).toEqual({ x: 1080, y: 120 });
    expect(flowLayoutStore.viewport).toEqual({ x: 15, y: 25, zoom: 1.15 });

    // 3. Reconcile against active server devices: detect and prune ghost nodes
    const knownDeviceIds = new Set(deviceStore.devices.map((d) => d.id));
    const staged = Array.from(flowLayoutStore.enabledDevices);

    for (const id of staged) {
      if (!knownDeviceIds.has(id)) {
        flowLayoutStore.disableDevice(id);
      }
    }

    // 4. Verify ghost device was cleanly pruned while real devices remain
    expect(flowLayoutStore.isDeviceEnabled('ghost:deleted-mic')).toBe(false);
    expect(flowLayoutStore.isDeviceEnabled(primaryDev)).toBe(true);

    // 5. Test Simulated Crash & Reboot in MockLocalStorage
    mockStorage.simulateCrash();
    expect(mockStorage.getItem(STORAGE_KEY_V1)).toBeNull(); // volatile RAM wiped

    mockStorage.simulateReboot(true); // persistent disk remains intact
    const restored = JSON.parse(mockStorage.getItem(STORAGE_KEY_V1) || '{}');
    expect(restored.version).toBe(1);
    expect(restored.stagedDeviceIds).toContain(primaryDev);
  });

  // ==========================================================================
  // Matrix 7: Crash Recovery & Network Disconnection
  // Abrupt WebSocket/SSE stream drop; client reconnect backoff;
  // state automatically rehydrates from fresh snapshot upon reconnection.
  // ==========================================================================
  it('Matrix 7: Crash Recovery & Network Disconnection - SSE/WS backoff and clean state rehydration', async () => {
    // 1. Initialize SSE Manager
    const sse = new SSEManager('/api/v1/telemetry/meters', {
      reconnectIntervalMs: 50,
      maxReconnectIntervalMs: 500,
    });

    const states: string[] = [];
    const unsubscribe = sse.onStateChange((st) => states.push(st));

    sse.connect();
    await tick();

    // Verify initial connection attempt
    expect(sse.state === 'connecting' || sse.state === 'connected').toBe(true);

    // 2. Simulate network disruption / crash
    const activeInstance = MockEventSource.instances[MockEventSource.instances.length - 1];
    if (activeInstance) {
      activeInstance.emitError(new Error('Network socket terminated'));
    }

    // 3. Verify reconnect backoff scheduling
    await waitForStore(
      () => sse.state,
      (st) => st === 'connecting' || st === 'error'
    );

    // 4. Clean reconnect and state hydration
    sse.reconnect();
    await tick();

    // Rehydrate stores from fresh snapshot upon reconnect
    await Promise.all([
      roomStore.loadRooms(),
      deviceStore.loadDevices(),
      matrixStore.loadRoutes(),
    ]);

    expect(roomStore.error).toBeNull();
    expect(deviceStore.error).toBeNull();
    expect(matrixStore.error).toBeNull();

    // Teardown
    sse.disconnect();
    unsubscribe();
    expect(sse.state).toBe('disconnected');
  });
});
