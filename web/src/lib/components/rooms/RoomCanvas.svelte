<script lang="ts">
  import { onMount, untrack } from 'svelte';
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

    // Initial default staging ONLY if canvas has never been configured
    if (!flowLayoutStore.hasSavedLayout) {
      ensureRoomDevicesStaged();
      ensureDefaultStaging();
    }
    syncFlowGraph();
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

  // Ensure devices bound to any room are staged on canvas
  function ensureRoomDevicesStaged() {
    for (const room of roomStore.rooms) {
      if (room.inputs) {
        for (const inId of room.inputs) {
          if (!flowLayoutStore.isDeviceEnabled(inId)) {
            flowLayoutStore.enableDevice(inId);
          }
        }
      }
      if (room.outputs) {
        for (const outId of room.outputs) {
          if (!flowLayoutStore.isDeviceEnabled(outId)) {
            flowLayoutStore.enableDevice(outId);
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
        for (const inId of room.inputs) defaultIds.add(inId);
      }
      if (room.outputs) {
        for (const outId of room.outputs) defaultIds.add(outId);
      }
    }

    // If still empty and hardware devices exist, stage primary local inputs and outputs
    if (defaultIds.size === 0 && deviceStore.devices.length > 0) {
      const inputs = deviceStore.inputDevices.slice(0, 2);
      const outputs = deviceStore.outputDevices.slice(0, 2);
      for (const d of inputs) defaultIds.add(d.id);
      for (const d of outputs) defaultIds.add(d.id);
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
        let devId = edge.source.replace('input-', '');
        const roomId = edge.target.replace('room-', '');
        if (devId.includes('/')) {
          devId = devId.slice(devId.indexOf('/') + 1);
        }
        await roomStore.removeRoomInput(roomId, devId);
      } else if (edge.source.startsWith('room-') && edge.target.startsWith('output-')) {
        const roomId = edge.source.replace('room-', '');
        let devId = edge.target.replace('output-', '');
        if (devId.includes('/')) {
          devId = devId.slice(devId.indexOf('/') + 1);
        }
        await roomStore.removeRoomOutput(roomId, devId);
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

  // Dismiss a node from the canvas: unbind dangling room routes and remove from flowLayoutStore
  async function handleDismissDevice(deviceId: string, direction?: 'input' | 'output') {
    // 1. Defensively unbind audio routes from roomStore
    for (const room of roomStore.rooms) {
      if (direction !== 'output' && room.inputs && (room.inputs.includes(deviceId) || room.inputs.some(i => i.endsWith(`/${deviceId}`)))) {
        await roomStore.removeRoomInput(room.id, deviceId);
      }
      if (direction !== 'input' && room.outputs && (room.outputs.includes(deviceId) || room.outputs.some(o => o.endsWith(`/${deviceId}`)))) {
        await roomStore.removeRoomOutput(room.id, deviceId);
      }
    }

    // 2. Remove device from persistent layout staging
    flowLayoutStore.disableDevice(deviceId);
    if (deviceId.includes('/')) {
      const clean = deviceId.slice(deviceId.indexOf('/') + 1);
      flowLayoutStore.disableDevice(clean);
    }

    // 3. Immediately persist to localStorage
    flowLayoutStore.persistNow();

    // 4. Re-synchronize canvas graph
    syncFlowGraph();
  }

  // HTML5 Drag-and-Drop handlers
  function handleDragOver(event: DragEvent) {
    event.preventDefault();
    if (event.dataTransfer) {
      event.dataTransfer.dropEffect = 'copy';
    }
  }

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
      const nodeId = isOutput ? `output-${canvasKey}` : `input-${canvasKey}`;

      flowLayoutStore.setPosition(nodeId, flowPos);
      flowLayoutStore.enableDevice(canvasKey);
      syncFlowGraph();
    } catch (err) {
      console.error('[RoomCanvas] Failed to process dropped device payload:', err);
    }
  }

  // Palette "+ Add" action handler
  function handleAddDevice(device: AudioDevice | AudioDeviceSummary, direction?: 'input' | 'output') {
    const isOutput = direction === 'output' || (device.max_output > 0 && device.max_input === 0);
    const devNodeId = (device as any).node_id || clusterStore.localNode?.node_id || '';
    const canvasKey = devNodeId && !device.id.includes('/') ? `${devNodeId}/${device.id}` : device.id;
    const nodeId = isOutput ? `output-${canvasKey}` : `input-${canvasKey}`;

    // Compute default column slot if no coordinate exists
    if (!flowLayoutStore.getPosition(nodeId) && !flowLayoutStore.getPosition(isOutput ? `output-${device.id}` : `input-${device.id}`)) {
      const stagedInputCount = nodes.filter((n) => n.type === 'inputDevice').length;
      const stagedOutputCount = nodes.filter((n) => n.type === 'outputDevice').length;
      const defPos = getDefaultPosition(
        isOutput ? 'output' : 'input',
        isOutput ? stagedOutputCount : stagedInputCount
      );
      flowLayoutStore.setPosition(nodeId, defPos);
    }

    // If duplex, ensure counterpart node position is also pre-allocated
    if (device.max_input > 0 && device.max_output > 0) {
      const otherNodeId = isOutput ? `input-${canvasKey}` : `output-${canvasKey}`;
      if (!flowLayoutStore.getPosition(otherNodeId) && !flowLayoutStore.getPosition(isOutput ? `input-${device.id}` : `output-${device.id}`)) {
        const count = nodes.filter((n) => n.type === (isOutput ? 'inputDevice' : 'outputDevice')).length;
        flowLayoutStore.setPosition(
          otherNodeId,
          getDefaultPosition(isOutput ? 'input' : 'output', count)
        );
      }
    }

    flowLayoutStore.enableDevice(canvasKey);
    syncFlowGraph();
  }

  // Palette "On Canvas" focus handler
  function handleFocusDevice(deviceId: string) {
    const targetNode = nodes.find((n) =>
      n.id === `input-${deviceId}` ||
      n.id === `output-${deviceId}` ||
      n.id.endsWith(`/${deviceId}`)
    );
    if (targetNode && svelteFlowInstance?.setCenter) {
      svelteFlowInstance.setCenter(targetNode.position.x + 144, targetNode.position.y + 90, {
        zoom: 1.0,
        duration: 400,
      });
    }
  }

  // Synchronize visual graph: ONLY renders nodes for staged devices present in flowLayoutStore
  function syncFlowGraph(forceResetPositions = false) {
    const allKnown = getAllKnownDevices();
    const stagedIds = flowLayoutStore.enabledDevices;
    const currentRooms = roomStore.rooms;

    const stagedDevs: (AudioDevice | AudioDeviceSummary)[] = [];
    for (const id of stagedIds) {
      const dev = allKnown.get(id);
      if (dev) {
        stagedDevs.push(dev);
      } else {
        let cleanId = id;
        let inferredNodeId = '';
        if (id.includes('/')) {
          const slash = id.indexOf('/');
          inferredNodeId = id.slice(0, slash);
          cleanId = id.slice(slash + 1);
        }
        const matchClean = allKnown.get(cleanId);
        if (matchClean) {
          stagedDevs.push({ ...matchClean, node_id: inferredNodeId || (matchClean as any).node_id });
          continue;
        }

        // Disconnected or offline device fallback
        const isVirt = cleanId.startsWith('virt:');
        const isKnownOutput =
          flowLayoutStore.getPosition(`output-${id}`) !== undefined ||
          flowLayoutStore.getPosition(`output-${cleanId}`) !== undefined ||
          currentRooms.some((r) => r.outputs && (r.outputs.includes(id) || r.outputs.includes(cleanId)));
        const isKnownInput =
          flowLayoutStore.getPosition(`input-${id}`) !== undefined ||
          flowLayoutStore.getPosition(`input-${cleanId}`) !== undefined ||
          currentRooms.some((r) => r.inputs && (r.inputs.includes(id) || r.inputs.includes(cleanId)));

        const maxIn = isKnownOutput && !isKnownInput ? 0 : (isVirt ? 2 : 1);
        const maxOut = isKnownInput && !isKnownOutput ? 0 : (isVirt ? 2 : 1);

        stagedDevs.push({
          id: cleanId,
          node_id: inferredNodeId,
          name: `${cleanId} (Offline / Disconnected)`,
          max_input: maxIn,
          max_output: maxOut,
          default_sample_rate: 48000,
          is_virtual: isVirt,
          volume: 1.0,
          muted: false,
        } as any);
      }
    }

    const inputDevs = stagedDevs.filter((d) => d.max_input > 0);
    const outputDevs = stagedDevs.filter((d) => d.max_output > 0);
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

      const isAssigned = (devId: string, nodePrefix: string, list?: string[]) => {
        if (!list) return false;
        if (nodePrefix && list.includes(`${nodePrefix}/${devId}`)) return true;
        return list.includes(devId) || list.some((item) => item.endsWith(`/${devId}`));
      };

      const assignedRooms = currentRooms
        .filter((r) => isAssigned(dev.id, devNodeId, r.inputs))
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
          onRemoveFromRoom: (roomId: string) => roomStore.removeRoomInput(roomId, dev.id),
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

      const isDevInList = (devId: string, nodePrefix: string, list?: string[]) => {
        if (!list) return false;
        if (nodePrefix && list.includes(`${nodePrefix}/${devId}`)) return true;
        return list.includes(devId) || list.some((item) => item.endsWith(`/${devId}`));
      };

      const assignedInputs = stagedDevs.filter(
        (d) => d.max_input > 0 && isDevInList(d.id, (d as any).node_id || '', room.inputs)
      );
      const assignedOutputs = stagedDevs.filter(
        (d) => d.max_output > 0 && isDevInList(d.id, (d as any).node_id || '', room.outputs)
      );

      const availableInputs = stagedDevs.filter(
        (d) => d.max_input > 0 && !isDevInList(d.id, (d as any).node_id || '', room.inputs)
      );
      const availableOutputs = stagedDevs.filter(
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
            const d = allKnown.get(devId);
            roomStore.addRoomInput(room.id, devId, (d as any)?.node_id);
          },
          onRemoveInput: (devId: string) => roomStore.removeRoomInput(room.id, devId),
          onAddOutput: (devId: string) => {
            const d = allKnown.get(devId);
            roomStore.addRoomOutput(room.id, devId, (d as any)?.node_id);
          },
          onRemoveOutput: (devId: string) => roomStore.removeRoomOutput(room.id, devId),
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
      // (a) Input -> Room
      if (room.inputs) {
        for (const inputDevId of room.inputs) {
          const matchDev = inputDevs.find((d) => {
            const devNodeId = (d as any).node_id || clusterStore.localNode?.node_id || '';
            const qualifiedId = devNodeId ? `${devNodeId}/${d.id}` : d.id;
            return qualifiedId === inputDevId || d.id === inputDevId || inputDevId.endsWith(`/${d.id}`);
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

      // (b) Room -> Output
      if (room.outputs) {
        for (const outputDevId of room.outputs) {
          const matchDev = outputDevs.find((d) => {
            const devNodeId = (d as any).node_id || clusterStore.localNode?.node_id || '';
            const qualifiedId = devNodeId ? `${devNodeId}/${d.id}` : d.id;
            return qualifiedId === outputDevId || d.id === outputDevId || outputDevId.endsWith(`/${d.id}`);
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

      const isDevAssigned = (devId: string, nodePrefix: string, list?: string[]) => {
        if (!list) return false;
        if (nodePrefix && list.includes(`${nodePrefix}/${devId}`)) return true;
        return list.includes(devId) || list.some((item) => item.endsWith(`/${devId}`));
      };

      const sourceRooms = currentRooms
        .filter((r) => isDevAssigned(dev.id, devNodeId, r.outputs))
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
          onRemoveFromRoom: (roomId: string) => roomStore.removeRoomOutput(roomId, dev.id),
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
      syncFlowGraph(false);
    });
  });

  const totalSummingPaths = $derived(
    roomStore.rooms.reduce((acc, r) => acc + (r.inputs?.length || 0) * (r.outputs?.length || 0), 0)
  );
</script>

<div class="relative flex h-full w-full bg-studio-dark select-none overflow-hidden">
  <!-- Left Side: Collapsible Multi-Node Device Palette (ADR 0012) -->
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
    {#if roomStore.lastError || deviceStore.error || invalidConnectionNotice}
      <div
        class="absolute top-20 left-1/2 -translate-x-1/2 z-50 w-11/12 max-w-2xl rounded-xl bg-red-950/95 border border-red-500/50 p-3 shadow-2xl backdrop-blur-md flex items-center justify-between gap-3 text-xs text-red-200 animate-in fade-in slide-in-from-top-2 duration-200"
      >
        <div class="flex items-center gap-2 min-w-0">
          <AlertCircle class="w-4 h-4 text-red-400 shrink-0" />
          <span class="truncate font-mono">
            {invalidConnectionNotice || roomStore.lastError || deviceStore.error}
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
      <Background gap={20} size={1.5} color="#27272a" />

      <!-- Canvas Navigation Controls -->
      <Controls class="!bg-zinc-900/90 !border !border-zinc-800 !text-zinc-200 !rounded-xl !shadow-xl overflow-hidden" />

      <!-- MiniMap for studio configurations -->
      <MiniMap
        nodeStrokeColor="#06b6d4"
        nodeColor="#18181b"
        maskColor="rgba(9, 9, 11, 0.8)"
        class="!bg-zinc-950 !border !border-zinc-800/90 !rounded-xl !shadow-2xl overflow-hidden"
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
