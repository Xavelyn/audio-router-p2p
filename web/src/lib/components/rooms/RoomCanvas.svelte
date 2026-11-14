<script lang="ts">
  import { onMount, onDestroy, untrack } from 'svelte';
  import {
    SvelteFlow,
    Controls,
    Background,
    MiniMap,
    Panel,
    useSvelteFlow,
    type Node,
    type Edge,
    type Connection,
    type Viewport,
  } from '@xyflow/svelte';
  import { roomStore } from '$lib/stores/rooms.svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import { clusterStore } from '$lib/stores/cluster.svelte';
  import { meterStore } from '$lib/stores/meters.svelte';
  import { telemetryStore } from '$lib/stores/telemetry.svelte';
  import { flowLayoutStore } from '$lib/stores/flowLayout.svelte';
  import type { AudioDevice, AudioDeviceSummary } from '$lib/api/types';
  import DevicePalette from './flow/DevicePalette.svelte';
  import InputDeviceNode from './flow/InputDeviceNode.svelte';
  import RoomBusNode from './flow/RoomBusNode.svelte';
  import OutputDeviceNode from './flow/OutputDeviceNode.svelte';
  import CreateRoomModal from './flow/CreateRoomModal.svelte';
  import CreateVirtualModal from './flow/CreateVirtualModal.svelte';
  import Plus from '@lucide/svelte/icons/plus';
  import Radio from '@lucide/svelte/icons/radio';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import X from '@lucide/svelte/icons/x';
  import Activity from '@lucide/svelte/icons/activity';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import Workflow from '@lucide/svelte/icons/workflow';
  import PanelLeftOpen from '@lucide/svelte/icons/panel-left-open';
  import PanelLeftClose from '@lucide/svelte/icons/panel-left-close';
  import Layers from '@lucide/svelte/icons/layers';

  const nodeTypes = {
    inputDevice: InputDeviceNode,
    roomBus: RoomBusNode,
    outputDevice: OutputDeviceNode,
  };

  let isCreateRoomOpen = $state(false);
  let isCreateVirtualOpen = $state(false);
  let invalidConnectionNotice = $state<string | null>(null);

  let nodes = $state<Node[]>([]);
  let edges = $state<Edge[]>([]);
  let viewport = $state<Viewport>(flowLayoutStore.viewport || { x: 0, y: 0, zoom: 1 });

  let flowContainer: HTMLDivElement | null = $state(null);

  // Hook into SvelteFlow instance if available
  let svelteFlowInstance: ReturnType<typeof useSvelteFlow> | null = null;
  try {
    svelteFlowInstance = useSvelteFlow();
  } catch {
    // Graceful fallback if SvelteFlowProvider is not yet active in parent context
  }

  onMount(async () => {
    await Promise.allSettled([
      roomStore.loadRooms(),
      deviceStore.loadDevices(),
      clusterStore.pollCluster(),
    ]);
    meterStore.init();

    // ADR 0034: the flow-canvas device HealthPills need the 2 s artifact
    // telemetry (stream-stats + device-artifacts). Start the per-page
    // polling here, mirroring TelemetryDashboard's convention.
    telemetryStore.startPolling(2000);

    // Initial default staging ONLY if canvas has never been configured
    if (!flowLayoutStore.hasSavedLayout) {
      ensureRoomDevicesStaged();
      ensureDefaultStaging();
    }
    syncFlowGraph();
  });

  onDestroy(() => {
    // Stop artifact telemetry when the flow canvas unmounts (tab switch).
    telemetryStore.stopPolling();
  });

  // Consolidate devices from local deviceStore and clusterStore
  function getAllKnownDevices(): Map<string, AudioDevice | AudioDeviceSummary> {
    const map = new Map<string, AudioDevice | AudioDeviceSummary>();
    const localNodeId = clusterStore.localNode?.node_id || clusterStore.localNode?.id || '';

    if (clusterStore.devices) {
      for (const d of clusterStore.devices) {
        const nodeId = d.node_id || localNodeId;
        const enriched = { ...d, node_id: nodeId };
        if (nodeId) {
          map.set(`${nodeId}/${d.id}`, enriched);
          if (d.device_id) map.set(`${nodeId}/${d.device_id}`, enriched);
          if (d.urn) map.set(`${nodeId}/${d.urn}`, enriched);
        }
        if (!map.has(d.id)) map.set(d.id, enriched);
        if (d.device_id && !map.has(d.device_id)) map.set(d.device_id, enriched);
        if (d.urn && !map.has(d.urn)) map.set(d.urn, enriched);
      }
    }
    if (deviceStore.devices) {
      for (const d of deviceStore.devices) {
        const nodeId = (d as any).node_id || localNodeId;
        const enriched = { ...d, node_id: nodeId };
        if (nodeId) {
          map.set(`${nodeId}/${d.id}`, enriched);
          if (d.urn) map.set(`${nodeId}/${d.urn}`, enriched);
        }
        if (!map.has(d.id)) map.set(d.id, enriched);
        if (d.urn && !map.has(d.urn)) map.set(d.urn, enriched);
      }
    }
    return map;
  }

  // Calculate default column coordinates for Left-to-Right signal flow with non-colliding margins
  function getDefaultPosition(type: 'input' | 'room' | 'output', index: number): { x: number; y: number } {
    switch (type) {
      case 'input':
        return { x: 50, y: 70 + index * 340 };
      case 'room':
        return { x: 480, y: 70 + index * 500 };
      case 'output':
        return { x: 1020, y: 70 + index * 340 };
    }
  }

  // Ensure devices bound to any room are staged on canvas (ADR 0027 directional staging)
  function ensureRoomDevicesStaged() {
    for (const room of roomStore.rooms) {
      if (room.inputs) {
        for (const inId of room.inputs) {
          if (!flowLayoutStore.isEndpointStaged(inId, 'input')) {
            flowLayoutStore.stageEndpoint(inId, 'input');
          }
        }
      }
      if (room.outputs) {
        for (const outId of room.outputs) {
          if (!flowLayoutStore.isEndpointStaged(outId, 'output')) {
            flowLayoutStore.stageEndpoint(outId, 'output');
          }
        }
      }
    }
  }

  // Ensure sensible defaults on first load (e.g. stage room-bound devices or default hardware)
  function ensureDefaultStaging() {
    if (flowLayoutStore.hasSavedLayout || flowLayoutStore.enabledDevices.size > 0) return;

    const defaultIds = new Set<string>();

    // Stage all devices currently participating in rooms
    for (const room of roomStore.rooms) {
      if (room.inputs) {
        for (const inId of room.inputs) defaultIds.add(`in:${inId}`);
      }
      if (room.outputs) {
        for (const outId of room.outputs) defaultIds.add(`out:${outId}`);
      }
    }

    // If still empty and hardware devices exist, stage primary local inputs and outputs
    if (defaultIds.size === 0 && deviceStore.devices.length > 0) {
      const inputs = deviceStore.inputDevices.slice(0, 2);
      const outputs = deviceStore.outputDevices.slice(0, 2);
      for (const d of inputs) defaultIds.add(`in:${d.id}`);
      for (const d of outputs) defaultIds.add(`out:${d.id}`);
    }

    if (defaultIds.size > 0) {
      flowLayoutStore.initDefaultsIfEmpty(Array.from(defaultIds));
    }
  }

  // Clear position cache and re-align staged devices and rooms in clean studio columns
  function handleAutoArrange() {
    flowLayoutStore.clearPositions();
    syncFlowGraph(true);
    if (svelteFlowInstance?.fitView) {
      setTimeout(() => svelteFlowInstance?.fitView?.({ padding: 0.2 }), 50);
    }
  }

  // Update persistent position when node dragging stops (supports single or multi-selection drags)
  function handleNodeDragStop({ targetNode, nodes: draggedNodes }: { targetNode: Node | null; nodes?: Node[] }) {
    if (targetNode) {
      flowLayoutStore.setPosition(targetNode.id, {
        x: targetNode.position.x,
        y: targetNode.position.y,
      });
    } else if (draggedNodes && draggedNodes.length > 0) {
      for (const n of draggedNodes) {
        flowLayoutStore.setPosition(n.id, {
          x: n.position.x,
          y: n.position.y,
        });
      }
    }
    flowLayoutStore.persistNow();
  }

  // Validate directional room-centric routing rules
  function isValidConnection(connection: Edge | Connection): boolean {
    if (!connection.source || !connection.target) return false;
    const isInputToRoom = connection.source.startsWith('input-') && connection.target.startsWith('room-');
    const isRoomToOutput = connection.source.startsWith('room-') && connection.target.startsWith('output-');
    return isInputToRoom || isRoomToOutput;
  }

  function handleConnect(connection: Connection) {
    invalidConnectionNotice = null;
    const allKnown = getAllKnownDevices();

    if (connection.source.startsWith('input-') && connection.target.startsWith('room-')) {
      const rawIn = connection.source.replace('input-', '');
      const roomId = connection.target.replace('room-', '');
      let devId = rawIn;
      let nodeId = '';
      if (rawIn.includes('/')) {
        const slash = rawIn.indexOf('/');
        nodeId = rawIn.slice(0, slash);
        devId = rawIn.slice(slash + 1);
      } else {
        const d = allKnown.get(devId);
        nodeId = (d as any)?.node_id || '';
      }
      roomStore.addRoomInput(roomId, devId, nodeId);
    } else if (connection.source.startsWith('room-') && connection.target.startsWith('output-')) {
      const roomId = connection.source.replace('room-', '');
      const rawOut = connection.target.replace('output-', '');
      let devId = rawOut;
      let nodeId = '';
      if (rawOut.includes('/')) {
        const slash = rawOut.indexOf('/');
        nodeId = rawOut.slice(0, slash);
        devId = rawOut.slice(slash + 1);
      } else {
        const d = allKnown.get(devId);
        nodeId = (d as any)?.node_id || '';
      }
      roomStore.addRoomOutput(roomId, devId, nodeId);
    } else {
      invalidConnectionNotice =
        'Direct connection rejected: In Room-Centric Audio Flow, connect Input devices to a Room, and the Room to Output devices.';
      setTimeout(() => {
        invalidConnectionNotice = null;
      }, 5000);
    }
  }

  // Canvas element deletion handler (handles both keyboard delete on nodes and edges)
  async function handleDelete({ nodes: deletedNodes = [], edges: deletedEdges = [] }: { nodes?: Node[]; edges?: Edge[] }) {
    // 1. Defensively handle deleted edges (disconnect audio routes)
    for (const edge of deletedEdges) {
      if (edge.source.startsWith('input-') && edge.target.startsWith('room-')) {
        const rawDevId = edge.source.replace('input-', '');
        const roomId = edge.target.replace('room-', '');
        const targetRoom = roomStore.rooms.find((r) => r.id === roomId);
        const matchId = targetRoom?.inputs?.find((i) => {
          if (i === rawDevId) return true;
          if (rawDevId.includes('/')) {
            const devPart = rawDevId.slice(rawDevId.indexOf('/') + 1);
            return !i.includes('/') && i === devPart;
          }
          return i.endsWith(`/${rawDevId}`);
        }) || rawDevId;
        try {
          await roomStore.removeRoomInput(roomId, matchId);
        } catch (err) {
          console.warn(`[RoomCanvas] Failed to unbind edge input ${matchId} from room ${roomId}:`, err);
        }
      } else if (edge.source.startsWith('room-') && edge.target.startsWith('output-')) {
        const roomId = edge.source.replace('room-', '');
        const rawDevId = edge.target.replace('output-', '');
        const targetRoom = roomStore.rooms.find((r) => r.id === roomId);
        const matchId = targetRoom?.outputs?.find((o) => {
          if (o === rawDevId) return true;
          if (rawDevId.includes('/')) {
            const devPart = rawDevId.slice(rawDevId.indexOf('/') + 1);
            return !o.includes('/') && o === devPart;
          }
          return o.endsWith(`/${rawDevId}`);
        }) || rawDevId;
        try {
          await roomStore.removeRoomOutput(roomId, matchId);
        } catch (err) {
          console.warn(`[RoomCanvas] Failed to unbind edge output ${matchId} from room ${roomId}:`, err);
        }
      }
    }

    // 2. Defensively handle deleted nodes (input, output, or room nodes)
    for (const node of deletedNodes) {
      if (node.id.startsWith('input-')) {
        const devId = node.id.replace('input-', '');
        await handleDismissDevice(devId, 'input');
      } else if (node.id.startsWith('output-')) {
        const devId = node.id.replace('output-', '');
        await handleDismissDevice(devId, 'output');
      } else if (node.id.startsWith('room-')) {
        const roomId = node.id.replace('room-', '');
        flowLayoutStore.deleteNodePosition(node.id);
        try {
          await roomStore.deleteRoom(roomId);
        } catch (err) {
          console.error(`Failed to delete room ${roomId}:`, err);
        }
      }
    }

    flowLayoutStore.persistNow();
    syncFlowGraph();
  }

  // Dismiss a node from the canvas: unbind dangling room routes and unstage strictly by direction and node (ADR 0027, ADR 0028)
  async function handleDismissDevice(deviceId: string, direction?: 'input' | 'output') {
    const isTargetMatch = (item: string) => {
      if (item === deviceId) return true;
      if (deviceId.includes('/')) {
        const clean = deviceId.slice(deviceId.indexOf('/') + 1);
        return !item.includes('/') && item === clean;
      }
      return item.endsWith(`/${deviceId}`);
    };

    // 1. Defensively unbind audio routes from roomStore (strictly direction-scoped and node-isolated per ADR 0027, ADR 0028)
    for (const room of roomStore.rooms) {
      if (direction === 'input') {
        const matched = room.inputs?.find(isTargetMatch);
        if (matched) {
          try {
            await roomStore.removeRoomInput(room.id, matched);
          } catch (err) {
            console.warn(`[RoomCanvas] Failed to unbind input route ${matched} from room ${room.id}:`, err);
          }
        }
      } else if (direction === 'output') {
        const matched = room.outputs?.find(isTargetMatch);
        if (matched) {
          try {
            await roomStore.removeRoomOutput(room.id, matched);
          } catch (err) {
            console.warn(`[RoomCanvas] Failed to unbind output route ${matched} from room ${room.id}:`, err);
          }
        }
      } else {
        // Direction not specified: defensive unbind for both
        const matchedIn = room.inputs?.find(isTargetMatch);
        if (matchedIn) {
          try {
            await roomStore.removeRoomInput(room.id, matchedIn);
          } catch (err) {
            console.warn(`[RoomCanvas] Failed to unbind input route ${matchedIn} from room ${room.id}:`, err);
          }
        }
        const matchedOut = room.outputs?.find(isTargetMatch);
        if (matchedOut) {
          try {
            await roomStore.removeRoomOutput(room.id, matchedOut);
          } catch (err) {
            console.warn(`[RoomCanvas] Failed to unbind output route ${matchedOut} from room ${room.id}:`, err);
          }
        }
      }
    }

    // 2. Unstage directional endpoint or disable device strictly for this deviceId (NO cleanKey unstage wipeout)
    if (direction) {
      flowLayoutStore.unstageEndpoint(deviceId, direction);
    } else {
      flowLayoutStore.disableDevice(deviceId);
    }

    // 3. Immediately persist to localStorage
    flowLayoutStore.persistNow();

    // 4. Re-synchronize canvas graph
    syncFlowGraph();
  }

  // HTML5 Drag-and-Drop handlers
  function handleDrop(event: DragEvent) {
    event.preventDefault();
    const rawData = event.dataTransfer?.getData('application/json');
    if (!rawData) return;

    try {
      const data = JSON.parse(rawData);
      const deviceId = data.deviceId;
      if (!deviceId) return;
      const devNodeId = data.nodeId || clusterStore.localNode?.node_id || '';

      // Project screen coordinates to SvelteFlow canvas coordinates
      let flowPos: { x: number; y: number };
      if (svelteFlowInstance?.screenToFlowPosition) {
        flowPos = svelteFlowInstance.screenToFlowPosition({
          x: event.clientX,
          y: event.clientY,
        });
      } else if (flowContainer && viewport) {
        const bounds = flowContainer.getBoundingClientRect();
        flowPos = {
          x: (event.clientX - bounds.left - viewport.x) / viewport.zoom,
          y: (event.clientY - bounds.top - viewport.y) / viewport.zoom,
        };
      } else {
        flowPos = { x: 100, y: 100 };
      }

      // If user dropped on the right side of the canvas (x > 750) or device is an output, target the output node
      const isOutput = data.direction === 'output' || (data.direction === 'duplex' && flowPos.x > 750);
      const canvasKey = devNodeId && !deviceId.includes('/') ? `${devNodeId}/${deviceId}` : deviceId;
      const dir: 'input' | 'output' = isOutput ? 'output' : 'input';

      flowLayoutStore.stageEndpoint(canvasKey, dir, flowPos);
      syncFlowGraph();
    } catch (err) {
      console.error('[RoomCanvas] Failed to process dropped device payload:', err);
    }
  }

  function handleDragOver(event: DragEvent) {
    event.preventDefault();
    if (event.dataTransfer) {
      event.dataTransfer.dropEffect = 'copy';
    }
  }

  // Palette "+ Add" action handler (ADR 0027 directional staging)
  function handleAddDevice(device: AudioDevice | AudioDeviceSummary, direction?: 'input' | 'output') {
    const isOutput = direction === 'output' || (device.max_output > 0 && device.max_input === 0);
    const isInput = direction === 'input' || (device.max_input > 0 && device.max_output === 0);
    const devNodeId = (device as any).node_id || clusterStore.localNode?.node_id || '';
    const canvasKey = devNodeId && !device.id.includes('/') ? `${devNodeId}/${device.id}` : device.id;

    if (direction) {
      const nodeId = direction === 'output' ? `output-${canvasKey}` : `input-${canvasKey}`;
      let pos = flowLayoutStore.getPosition(nodeId) || flowLayoutStore.getPosition(direction === 'output' ? `output-${device.id}` : `input-${device.id}`);
      if (!pos) {
        const count = nodes.filter((n) => n.type === (direction === 'output' ? 'outputDevice' : 'inputDevice')).length;
        pos = getDefaultPosition(direction, count);
      }
      flowLayoutStore.stageEndpoint(canvasKey, direction, pos);
    } else if (isOutput) {
      const nodeId = `output-${canvasKey}`;
      let pos = flowLayoutStore.getPosition(nodeId) || flowLayoutStore.getPosition(`output-${device.id}`);
      if (!pos) {
        const count = nodes.filter((n) => n.type === 'outputDevice').length;
        pos = getDefaultPosition('output', count);
      }
      flowLayoutStore.stageEndpoint(canvasKey, 'output', pos);
    } else if (isInput) {
      const nodeId = `input-${canvasKey}`;
      let pos = flowLayoutStore.getPosition(nodeId) || flowLayoutStore.getPosition(`input-${device.id}`);
      if (!pos) {
        const count = nodes.filter((n) => n.type === 'inputDevice').length;
        pos = getDefaultPosition('input', count);
      }
      flowLayoutStore.stageEndpoint(canvasKey, 'input', pos);
    } else {
      // Duplex device with unspecified direction: stage both endpoints
      const inNodeId = `input-${canvasKey}`;
      let inPos = flowLayoutStore.getPosition(inNodeId) || flowLayoutStore.getPosition(`input-${device.id}`);
      if (!inPos) {
        const count = nodes.filter((n) => n.type === 'inputDevice').length;
        inPos = getDefaultPosition('input', count);
      }
      flowLayoutStore.stageEndpoint(canvasKey, 'input', inPos);

      const outNodeId = `output-${canvasKey}`;
      let outPos = flowLayoutStore.getPosition(outNodeId) || flowLayoutStore.getPosition(`output-${device.id}`);
      if (!outPos) {
        const count = nodes.filter((n) => n.type === 'outputDevice').length;
        outPos = getDefaultPosition('output', count);
      }
      flowLayoutStore.stageEndpoint(canvasKey, 'output', outPos);
    }

    syncFlowGraph();
  }

  // Palette "On Canvas" focus handler with directional awareness (ADR 0027, ADR 0028)
  function handleFocusDevice(deviceId: string, direction?: 'input' | 'output') {
    const isTargetMatch = (nId: string, prefix: string) => {
      const stripped = nId.slice(prefix.length);
      if (stripped === deviceId) return true;
      if (deviceId.includes('/')) {
        return false;
      }
      return stripped.endsWith(`/${deviceId}`);
    };

    const targetNode = nodes.find((n) => {
      if (direction === 'input') {
        return n.id.startsWith('input-') && isTargetMatch(n.id, 'input-');
      }
      if (direction === 'output') {
        return n.id.startsWith('output-') && isTargetMatch(n.id, 'output-');
      }
      if (n.id.startsWith('input-')) return isTargetMatch(n.id, 'input-');
      if (n.id.startsWith('output-')) return isTargetMatch(n.id, 'output-');
      return n.id === deviceId;
    });

    if (targetNode && svelteFlowInstance?.setCenter) {
      svelteFlowInstance.setCenter(targetNode.position.x + 144, targetNode.position.y + 90, {
        zoom: 1.0,
        duration: 400,
      });
    }
  }

  // Helper to resolve an endpoint device from allKnown, or synthesize an offline fallback (ADR 0027, ADR 0028)
  function resolveEndpointDevice(
    rawId: string,
    direction: 'input' | 'output',
    allKnown: Map<string, AudioDevice | AudioDeviceSummary>
  ): AudioDevice | AudioDeviceSummary {
    let cleanId = rawId;
    let inferredNodeId = '';
    if (rawId.includes('/')) {
      const slash = rawId.indexOf('/');
      inferredNodeId = rawId.slice(0, slash);
      cleanId = rawId.slice(slash + 1);
    }

    // If rawId has node qualification, match allKnown.get(rawId) ONLY.
    // Do NOT fall back to cleanId if rawId is qualified, because cleanId might belong to another node!
    const match = rawId.includes('/') ? allKnown.get(rawId) : (allKnown.get(rawId) || allKnown.get(cleanId));
    if (match) {
      return {
        ...match,
        node_id: inferredNodeId || (match as any).node_id || clusterStore.localNode?.node_id || '',
      };
    }

    // Synthetic Fallback Descriptor for offline/unstaged devices (ADR 0027, ADR 0028)
    const isVirt = cleanId.startsWith('virt:');
    return {
      id: cleanId,
      node_id: inferredNodeId || clusterStore.localNode?.node_id || '',
      name: `${cleanId} (Offline / Unstaged)`,
      max_input: direction === 'input' ? (isVirt ? 2 : 1) : 0,
      max_output: direction === 'output' ? (isVirt ? 2 : 1) : 0,
      default_sample_rate: 48000,
      is_virtual: isVirt,
      volume: 1.0,
      muted: false,
    } as AudioDeviceSummary;
  }

  // Synchronize visual graph: ONLY renders nodes for staged directional endpoints present in flowLayoutStore
  // or bound to active rooms (ADR 0027, ADR 0028)
  function syncFlowGraph(forceResetPositions = false) {
    const allKnown = getAllKnownDevices();
    const currentRooms = roomStore.rooms;

    const isDevInList = (devId: string, nodePrefix: string, list?: string[]) => {
      if (!list || list.length === 0) return false;
      const qualified = nodePrefix && !devId.includes('/') ? `${nodePrefix}/${devId}` : devId;
      for (const item of list) {
        if (item === qualified) return true;
        if (item.includes('/')) {
          if (qualified.includes('/')) continue; // Both qualified but different nodes: no match (ADR 0028)
          if (item.endsWith(`/${devId}`)) return true;
        } else {
          if (item === devId) return true;
        }
      }
      return false;
    };

    // Gather unique candidate devices across allKnown, room bindings, and flowLayoutStore staging
    const candidateMap = new Map<string, AudioDevice | AudioDeviceSummary>();
    for (const [key, dev] of allKnown) {
      candidateMap.set(key, dev);
    }

    // Include devices from rooms
    for (const room of currentRooms) {
      if (room.inputs) {
        for (const inId of room.inputs) {
          const existing = candidateMap.get(inId);
          if (!existing) {
            candidateMap.set(inId, resolveEndpointDevice(inId, 'input', allKnown));
          } else if (existing.max_input === 0) {
            existing.max_input = existing.is_virtual ? 2 : 1;
          }
        }
      }
      if (room.outputs) {
        for (const outId of room.outputs) {
          const existing = candidateMap.get(outId);
          if (!existing) {
            candidateMap.set(outId, resolveEndpointDevice(outId, 'output', allKnown));
          } else if (existing.max_output === 0) {
            existing.max_output = existing.is_virtual ? 2 : 1;
          }
        }
      }
    }

    // Include devices from flowLayoutStore staging
    for (const rawId of flowLayoutStore.enabledDevices) {
      const clean = rawId.replace(/^(in:|out:|input-|output-)/, '');
      const dir = rawId.startsWith('out:') || rawId.startsWith('output-') ? 'output' : 'input';
      const existing = candidateMap.get(clean);
      if (!existing) {
        candidateMap.set(clean, resolveEndpointDevice(clean, dir, allKnown));
      } else if (dir === 'input' && existing.max_input === 0) {
        existing.max_input = existing.is_virtual ? 2 : 1;
      } else if (dir === 'output' && existing.max_output === 0) {
        existing.max_output = existing.is_virtual ? 2 : 1;
      }
    }

    // Filter candidate devices to inputDevs and outputDevs based on directional staging
    const inputDevs: (AudioDevice | AudioDeviceSummary)[] = [];
    const outputDevs: (AudioDevice | AudioDeviceSummary)[] = [];

    const seenInputKeys = new Set<string>();
    const seenOutputKeys = new Set<string>();

    for (const dev of candidateMap.values()) {
      const devNodeId = (dev as any).node_id || clusterStore.localNode?.node_id || '';
      const canvasKey = devNodeId && !dev.id.includes('/') ? `${devNodeId}/${dev.id}` : dev.id;

      // Check input staging: staged in store OR bound to any room input (ADR 0028 strict canonical)
      const isInputStaged =
        dev.max_input > 0 &&
        (flowLayoutStore.isEndpointStaged(canvasKey, 'input') ||
          currentRooms.some((r) => isDevInList(dev.id, devNodeId, r.inputs)));

      if (isInputStaged && !seenInputKeys.has(canvasKey)) {
        seenInputKeys.add(canvasKey);
        inputDevs.push(dev);
      }

      // Check output staging: staged in store OR bound to any room output (ADR 0028 strict canonical)
      const isOutputStaged =
        dev.max_output > 0 &&
        (flowLayoutStore.isEndpointStaged(canvasKey, 'output') ||
          currentRooms.some((r) => isDevInList(dev.id, devNodeId, r.outputs)));

      if (isOutputStaged && !seenOutputKeys.has(canvasKey)) {
        seenOutputKeys.add(canvasKey);
        outputDevs.push(dev);
      }
    }

    const roomSummaries = currentRooms.map((r) => ({ id: r.id, name: r.name }));
    const nextNodes: Node[] = [];
    const nextEdges: Edge[] = [];

    // 1. Generate Staged Input Nodes (Left Column)
    inputDevs.forEach((dev, index) => {
      const devNodeId = (dev as any).node_id || clusterStore.localNode?.node_id || '';
      const canvasKey = devNodeId && !dev.id.includes('/') ? `${devNodeId}/${dev.id}` : dev.id;
      const nodeId = `input-${canvasKey}`;
      let pos = forceResetPositions ? null : (flowLayoutStore.getPosition(nodeId) || flowLayoutStore.getPosition(`input-${dev.id}`));
      if (!pos) {
        pos = getDefaultPosition('input', index);
        flowLayoutStore.setPosition(nodeId, pos);
      }

      const assignedRooms = currentRooms
        .filter((r) => isDevInList(dev.id, devNodeId, r.inputs))
        .map((r) => ({ id: r.id, name: r.name }));

      nextNodes.push({
        id: nodeId,
        type: 'inputDevice',
        position: { x: pos.x, y: pos.y },
        data: {
          device: dev,
          assignedRooms,
          allRooms: roomSummaries,
          onAddToRoom: (roomId: string) => roomStore.addRoomInput(roomId, dev.id, devNodeId),
          onRemoveFromRoom: (roomId: string) => roomStore.removeRoomInput(roomId, canvasKey),
          onDismiss: () => handleDismissDevice(canvasKey, 'input'),
        },
      });
    });

    // 2. Generate Room Nodes (Center Column)
    currentRooms.forEach((room, index) => {
      const nodeId = `room-${room.id}`;
      let pos = forceResetPositions ? null : flowLayoutStore.getPosition(nodeId);
      if (!pos) {
        pos = getDefaultPosition('room', index);
        flowLayoutStore.setPosition(nodeId, pos);
      }

      // ADR 0027, ADR 0028: Decouple assignedInputs and assignedOutputs from stagedDevs!
      // Resolve every device in room.inputs and room.outputs directly from allKnown (or synthetic fallback).
      // Guarantees room.inputs.length === assignedInputs.length 1:1, preserving canonical endpoint_id.
      const assignedInputs = (room.inputs || []).map((inId) => {
        const d = resolveEndpointDevice(inId, 'input', allKnown);
        return { ...d, endpoint_id: inId };
      });
      const assignedOutputs = (room.outputs || []).map((outId) => {
        const d = resolveEndpointDevice(outId, 'output', allKnown);
        return { ...d, endpoint_id: outId };
      });

      // Available inputs/outputs for the "+ Input" / "+ Output" dropdown menus
      const uniqueKnown = Array.from(new Set(allKnown.values()));
      const availableInputs = uniqueKnown.filter(
        (d) => d.max_input > 0 && !isDevInList(d.id, (d as any).node_id || '', room.inputs)
      );
      const availableOutputs = uniqueKnown.filter(
        (d) => d.max_output > 0 && !isDevInList(d.id, (d as any).node_id || '', room.outputs)
      );

      nextNodes.push({
        id: nodeId,
        type: 'roomBus',
        position: { x: pos.x, y: pos.y },
        data: {
          room,
          inputDevices: assignedInputs,
          outputDevices: assignedOutputs,
          availableInputs,
          availableOutputs,
          onToggleSync: (synced: boolean) => roomStore.setRoomSync(room.id, synced),
          onAddInput: (devId: string) => {
            let devNodeId = '';
            let targetDev = devId;
            if (devId.includes('/')) {
              devNodeId = devId.slice(0, devId.indexOf('/'));
              targetDev = devId.slice(devId.indexOf('/') + 1);
            } else {
              const d = allKnown.get(devId);
              devNodeId = (d as any)?.node_id || '';
            }
            roomStore.addRoomInput(room.id, targetDev, devNodeId);
          },
          onRemoveInput: (endpointId: string) => roomStore.removeRoomInput(room.id, endpointId),
          onAddOutput: (devId: string) => {
            let devNodeId = '';
            let targetDev = devId;
            if (devId.includes('/')) {
              devNodeId = devId.slice(0, devId.indexOf('/'));
              targetDev = devId.slice(devId.indexOf('/') + 1);
            } else {
              const d = allKnown.get(devId);
              devNodeId = (d as any)?.node_id || '';
            }
            roomStore.addRoomOutput(room.id, targetDev, devNodeId);
          },
          onRemoveOutput: (endpointId: string) => roomStore.removeRoomOutput(room.id, endpointId),
          onDeleteRoom: async () => {
            if (confirm(`Delete audio room "${room.name}" (${room.id})?`)) {
              flowLayoutStore.deleteNodePosition(`room-${room.id}`);
              flowLayoutStore.persistNow();
              try {
                await roomStore.deleteRoom(room.id);
              } catch (err) {
                console.error(`Failed to delete room ${room.id}:`, err);
              }
              syncFlowGraph();
            }
          },
        },
      });

      // 3. Generate Edges for this Room
      // (a) Input -> Room (ADR 0028 strict canonical matching)
      if (room.inputs) {
        for (const inputDevId of room.inputs) {
          const matchDev = inputDevs.find((d) => {
            const devNodeId = (d as any).node_id || clusterStore.localNode?.node_id || '';
            const qualifiedId = devNodeId && !d.id.includes('/') ? `${devNodeId}/${d.id}` : d.id;
            if (qualifiedId === inputDevId) return true;
            if (inputDevId.includes('/')) return false;
            return d.id === inputDevId;
          });
          if (matchDev) {
            const devNodeId = (matchDev as any).node_id || clusterStore.localNode?.node_id || '';
            const canvasKey = devNodeId && !matchDev.id.includes('/') ? `${devNodeId}/${matchDev.id}` : matchDev.id;
            nextEdges.push({
              id: `edge-in-${canvasKey}-to-${room.id}`,
              source: `input-${canvasKey}`,
              target: `room-${room.id}`,
              sourceHandle: 'out',
              targetHandle: 'in',
              animated: true,
              style:
                'stroke: #06b6d4; stroke-width: 2.5px; filter: drop-shadow(0 0 5px rgba(6, 182, 212, 0.7));',
            });
          }
        }
      }

      // (b) Room -> Output (ADR 0028 strict canonical matching)
      if (room.outputs) {
        for (const outputDevId of room.outputs) {
          const matchDev = outputDevs.find((d) => {
            const devNodeId = (d as any).node_id || clusterStore.localNode?.node_id || '';
            const qualifiedId = devNodeId && !d.id.includes('/') ? `${devNodeId}/${d.id}` : d.id;
            if (qualifiedId === outputDevId) return true;
            if (outputDevId.includes('/')) return false;
            return d.id === outputDevId;
          });
          if (matchDev) {
            const devNodeId = (matchDev as any).node_id || clusterStore.localNode?.node_id || '';
            const canvasKey = devNodeId && !matchDev.id.includes('/') ? `${devNodeId}/${matchDev.id}` : matchDev.id;
            nextEdges.push({
              id: `edge-room-${room.id}-to-out-${canvasKey}`,
              source: `room-${room.id}`,
              target: `output-${canvasKey}`,
              sourceHandle: 'out',
              targetHandle: 'in',
              animated: true,
              style:
                'stroke: #a855f7; stroke-width: 2.5px; filter: drop-shadow(0 0 5px rgba(168, 85, 247, 0.7));',
            });
          }
        }
      }
    });

    // 4. Generate Staged Output Nodes (Right Column)
    outputDevs.forEach((dev, index) => {
      const devNodeId = (dev as any).node_id || clusterStore.localNode?.node_id || '';
      const canvasKey = devNodeId && !dev.id.includes('/') ? `${devNodeId}/${dev.id}` : dev.id;
      const nodeId = `output-${canvasKey}`;
      let pos = forceResetPositions ? null : (flowLayoutStore.getPosition(nodeId) || flowLayoutStore.getPosition(`output-${dev.id}`));
      if (!pos) {
        pos = getDefaultPosition('output', index);
        flowLayoutStore.setPosition(nodeId, pos);
      }

      const sourceRooms = currentRooms
        .filter((r) => isDevInList(dev.id, devNodeId, r.outputs))
        .map((r) => ({ id: r.id, name: r.name }));

      nextNodes.push({
        id: nodeId,
        type: 'outputDevice',
        position: { x: pos.x, y: pos.y },
        data: {
          device: dev,
          sourceRooms,
          allRooms: roomSummaries,
          onAddToRoom: (roomId: string) => roomStore.addRoomOutput(roomId, dev.id, devNodeId),
          onRemoveFromRoom: (roomId: string) => roomStore.removeRoomOutput(roomId, canvasKey),
          onDismiss: () => handleDismissDevice(canvasKey, 'output'),
        },
      });
    });

    nodes = nextNodes;
    edges = nextEdges;
  }

  // Reactive effect: Rebuild graph whenever rooms, devices, or layout stage updates
  $effect(() => {
    const _rooms = roomStore.rooms;
    const _devices = deviceStore.devices;
    const _clusterDevices = clusterStore.devices;
    const _staged = flowLayoutStore.enabledDevices;

    untrack(() => {
      // If canvas has never been configured and rooms/devices just arrived (e.g. after initial gateway disconnect), stage defaults
      if (!flowLayoutStore.hasSavedLayout && flowLayoutStore.enabledDevices.size === 0 && roomStore.rooms.length > 0) {
        ensureRoomDevicesStaged();
        ensureDefaultStaging();
      }
      syncFlowGraph(false);
    });
  });

  const totalSummingPaths = $derived(
    roomStore.rooms.reduce((acc, r) => acc + (r.inputs?.length || 0) * (r.outputs?.length || 0), 0)
  );
</script>

<div class="relative flex h-full w-full bg-studio-dark select-none overflow-hidden">
  <!-- Left Side: Collapsible Multi-Node Device Palette (ADR 0012, ADR 0027) -->
  {#if flowLayoutStore.sidebarOpen}
    <DevicePalette
      onAddDevice={handleAddDevice}
      onFocusDevice={handleFocusDevice}
    />
  {/if}

  <!-- Right Side: Svelte Flow Canvas Drop Zone -->
  <div
    class="relative flex-1 h-full overflow-hidden"
    bind:this={flowContainer}
    ondragover={handleDragOver}
    ondrop={handleDrop}
    role="region"
    aria-label="Audio Flow Canvas"
  >
    <!-- Prominent Sticky Error Notification Banner -->
    {#if roomStore.lastError || roomStore.error || deviceStore.error || invalidConnectionNotice}
      <div
        class="absolute top-20 left-1/2 -translate-x-1/2 z-50 w-11/12 max-w-2xl rounded-xl bg-red-950/95 border border-red-500/50 p-3 shadow-2xl backdrop-blur-md flex items-center justify-between gap-3 text-xs text-red-200 animate-in fade-in slide-in-from-top-2 duration-200"
      >
        <div class="flex items-center gap-2 min-w-0">
          <AlertCircle class="w-4 h-4 text-red-400 shrink-0" />
          <span class="truncate font-mono">
            {invalidConnectionNotice || roomStore.lastError || roomStore.error || deviceStore.error}
          </span>
        </div>
        <button
          type="button"
          onclick={() => {
            roomStore.clearLastError();
            deviceStore.error = null;
            invalidConnectionNotice = null;
          }}
          class="rounded p-1 text-red-400 hover:text-white hover:bg-red-900/50 transition-colors"
          aria-label="Dismiss error banner"
        >
          <X class="w-4 h-4" />
        </button>
      </div>
    {/if}

    <!-- Svelte Flow Canvas -->
    <SvelteFlow
      bind:nodes
      bind:edges
      bind:viewport
      {nodeTypes}
      fitView={!flowLayoutStore.viewport}
      minZoom={0.2}
      maxZoom={2.0}
      onnodedragstop={handleNodeDragStop}
      onmoveend={({ viewport: vp }) => {
        if (vp) flowLayoutStore.setViewport(vp);
      }}
      onconnect={handleConnect}
      ondelete={handleDelete}
      {isValidConnection}
      class="bg-zinc-950"
    >
      <!-- Background grid dots -->
      <Background color="#27272a" gap={20} size={1.5} />

      <!-- Controls & Minimap -->
      <Controls class="!bg-zinc-900/90 !border !border-zinc-800 !rounded-xl !shadow-xl !fill-zinc-300" />
      <MiniMap
        class="!bg-zinc-900/90 !border !border-zinc-800 !rounded-xl !shadow-xl"
        nodeColor="#3f3f46"
        maskColor="rgba(9, 9, 11, 0.75)"
      />

      <!-- Top Left Information & Palette Toggle Panel -->
      <Panel position="top-left" class="m-3">
        <div class="rounded-2xl border border-zinc-800/90 bg-zinc-950/85 backdrop-blur-md p-3.5 shadow-2xl flex flex-col gap-2 max-w-sm">
          <div class="flex items-center justify-between gap-3">
            <div class="flex items-center gap-2">
              <div class="flex h-7 w-7 items-center justify-center rounded-lg bg-cyan-500/10 border border-cyan-500/30 text-cyan-400">
                <Workflow class="w-4 h-4" />
              </div>
              <div>
                <h1 class="text-xs font-bold font-mono uppercase tracking-wider text-zinc-100 flex items-center gap-1.5">
                  <span>Studio Audio Flow</span>
                  <span class="rounded bg-cyan-500/20 px-1 py-0.2 text-[9px] text-cyan-300 border border-cyan-500/30">CANVAS</span>
                </h1>
                <p class="text-[10px] font-mono text-zinc-400">
                  {roomStore.rooms.length} Rooms &bull; {flowLayoutStore.enabledDevices.size} Staged &bull; <strong class="text-cyan-300 font-bold">{totalSummingPaths} Paths</strong>
                </p>
              </div>
            </div>

            <!-- Toggle Palette Drawer Button -->
            <button
              type="button"
              onclick={() => flowLayoutStore.toggleSidebar()}
              class="inline-flex items-center gap-1 rounded-lg border px-2 py-1 text-xs font-mono transition-all {flowLayoutStore.sidebarOpen
                ? 'border-zinc-700 bg-zinc-900 text-zinc-300 hover:text-white'
                : 'border-cyan-500/40 bg-cyan-500/10 text-cyan-300 hover:bg-cyan-500/20 shadow-sm shadow-cyan-500/20'}"
              title={flowLayoutStore.sidebarOpen ? 'Hide Device Palette' : 'Open Device Palette'}
            >
              {#if flowLayoutStore.sidebarOpen}
                <PanelLeftClose class="w-3.5 h-3.5 text-zinc-400" />
                <span class="hidden sm:inline text-[11px]">Hide</span>
              {:else}
                <PanelLeftOpen class="w-3.5 h-3.5 text-cyan-400" />
                <span class="text-[11px] font-bold">Palette</span>
              {/if}
            </button>
          </div>

          <div class="rounded-lg bg-zinc-900/60 p-2 text-[10px] font-sans text-zinc-400 leading-snug border border-zinc-800/80">
            <span class="text-cyan-300 font-semibold font-mono">Routing Rule:</span>
            Drag interfaces from the Device Palette onto the canvas. Connect Inputs to Rooms, and Rooms to Outputs.
          </div>
        </div>
      </Panel>

      <!-- Top Right Action Toolbar Panel -->
      <Panel position="top-right" class="m-3">
        <div class="flex flex-wrap items-center justify-end gap-1.5 sm:gap-2 rounded-2xl border border-zinc-800/90 bg-zinc-950/85 backdrop-blur-md p-2 shadow-2xl">
          <!-- Add Room Button -->
          <button
            type="button"
            onclick={() => (isCreateRoomOpen = true)}
            class="inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-2.5 sm:px-3 py-1.5 text-xs font-mono font-bold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 transition-all cursor-pointer shrink-0"
          >
            <Plus class="w-3.5 h-3.5" />
            <span class="hidden sm:inline">New Room</span>
            <span class="sm:hidden">Room</span>
          </button>

          <!-- Add Virtual Device Button -->
          <button
            type="button"
            onclick={() => (isCreateVirtualOpen = true)}
            class="inline-flex items-center gap-1.5 rounded-lg border border-purple-500/30 bg-purple-500/10 px-2.5 sm:px-3 py-1.5 text-xs font-mono font-semibold text-purple-300 hover:bg-purple-500/20 transition-all cursor-pointer shrink-0"
          >
            <Radio class="w-3.5 h-3.5 text-purple-400" />
            <span class="hidden md:inline">New Virtual Loopback</span>
            <span class="md:hidden">Virtual</span>
          </button>

          <!-- Auto-Arrange Layout Button -->
          <button
            type="button"
            onclick={handleAutoArrange}
            class="inline-flex items-center gap-1.5 rounded-lg border border-zinc-700 bg-zinc-900 px-2.5 py-1.5 text-xs font-mono font-medium text-zinc-300 hover:bg-zinc-800 transition-colors cursor-pointer shrink-0"
            title="Reset layout and align nodes in Left-to-Right signal flow columns"
          >
            <RotateCcw class="w-3.5 h-3.5 text-cyan-400" />
            <span class="hidden sm:inline">Auto-Arrange</span>
          </button>

          <!-- Simulate Metering Toggle -->
          <button
            type="button"
            onclick={() => meterStore.toggleSimulation()}
            class="inline-flex items-center gap-1 rounded-lg border px-2.5 py-1.5 text-xs font-mono transition-colors cursor-pointer shrink-0 {meterStore.useSimulation
              ? 'border-emerald-500/50 bg-emerald-950/30 text-emerald-300'
              : 'border-zinc-800 bg-zinc-900 text-zinc-400 hover:text-zinc-200'}"
            title="Toggle audio signal simulation for live LED meters"
          >
            <Activity class="w-3.5 h-3.5 {meterStore.useSimulation ? 'text-emerald-400 animate-pulse' : 'text-zinc-500'}" />
            <span class="hidden md:inline">{meterStore.useSimulation ? 'Sim ON' : 'Sim Meters'}</span>
          </button>

          <!-- Refresh Button -->
          <button
            type="button"
            onclick={() => {
              roomStore.loadRooms();
              deviceStore.loadDevices();
              clusterStore.pollCluster();
            }}
            class="inline-flex items-center justify-center h-8 w-8 rounded-lg border border-zinc-800 bg-zinc-900 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-800 transition-colors cursor-pointer shrink-0"
            title="Reload devices and rooms"
            aria-label="Reload devices and rooms"
          >
            <RefreshCw class="w-3.5 h-3.5 {roomStore.isLoading ? 'animate-spin text-cyan-400' : ''}" />
          </button>
        </div>
      </Panel>
    </SvelteFlow>
  </div>

  <!-- Modals -->
  <CreateRoomModal bind:isOpen={isCreateRoomOpen} />
  <CreateVirtualModal bind:isOpen={isCreateVirtualOpen} />
</div>
