<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import {
    SvelteFlow,
    Controls,
    Background,
    MiniMap,
    Panel,
    type Node,
    type Edge,
    type Connection,
  } from '@xyflow/svelte';
  import { roomStore } from '$lib/stores/rooms.svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import { meterStore } from '$lib/stores/meters.svelte';
  import InputDeviceNode from './flow/InputDeviceNode.svelte';
  import RoomBusNode from './flow/RoomBusNode.svelte';
  import OutputDeviceNode from './flow/OutputDeviceNode.svelte';
  import CreateRoomModal from './flow/CreateRoomModal.svelte';
  import CreateVirtualModal from './flow/CreateVirtualModal.svelte';
  import Plus from '@lucide/svelte/icons/plus';
  import Network from '@lucide/svelte/icons/network';
  import Radio from '@lucide/svelte/icons/radio';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import X from '@lucide/svelte/icons/x';
  import Sliders from '@lucide/svelte/icons/sliders';
  import Activity from '@lucide/svelte/icons/activity';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import Workflow from '@lucide/svelte/icons/workflow';

  const nodeTypes = {
    inputDevice: InputDeviceNode,
    roomBus: RoomBusNode,
    outputDevice: OutputDeviceNode,
  };

  let isCreateRoomOpen = $state(false);
  let isCreateVirtualOpen = $state(false);
  let invalidConnectionNotice = $state<string | null>(null);

  // Position cache so moving nodes around on canvas is preserved
  const nodePositionCache = new Map<string, { x: number; y: number }>();

  let nodes = $state<Node[]>([]);
  let edges = $state<Edge[]>([]);

  onMount(() => {
    roomStore.loadRooms();
    deviceStore.loadDevices();
    meterStore.init();
  });

  // Calculate default column coordinates
  function getDefaultPosition(type: 'input' | 'room' | 'output', index: number): { x: number; y: number } {
    switch (type) {
      case 'input':
        return { x: 50, y: 70 + index * 270 };
      case 'room':
        return { x: 440, y: 70 + index * 420 };
      case 'output':
        return { x: 970, y: 70 + index * 270 };
    }
  }

  // Clear position cache and re-align in clean studio columns
  function handleAutoArrange() {
    nodePositionCache.clear();
    syncFlowGraph(true);
  }

  function handleNodeDragStop({ targetNode }: { targetNode: Node | null }) {
    if (targetNode) {
      nodePositionCache.set(targetNode.id, {
        x: targetNode.position.x,
        y: targetNode.position.y,
      });
    }
  }

  function isValidConnection(connection: Edge | Connection): boolean {
    if (!connection.source || !connection.target) return false;
    const isInputToRoom = connection.source.startsWith('input-') && connection.target.startsWith('room-');
    const isRoomToOutput = connection.source.startsWith('room-') && connection.target.startsWith('output-');
    return isInputToRoom || isRoomToOutput;
  }

  function handleConnect(connection: Connection) {
    invalidConnectionNotice = null;

    if (connection.source.startsWith('input-') && connection.target.startsWith('room-')) {
      const devId = connection.source.replace('input-', '');
      const roomId = connection.target.replace('room-', '');
      roomStore.addRoomInput(roomId, devId);
    } else if (connection.source.startsWith('room-') && connection.target.startsWith('output-')) {
      const roomId = connection.source.replace('room-', '');
      const devId = connection.target.replace('output-', '');
      roomStore.addRoomOutput(roomId, devId);
    } else {
      invalidConnectionNotice =
        'Direct connection rejected: In Room-Centric Audio Flow, connect Input devices to a Room, and the Room to Output devices.';
      setTimeout(() => {
        invalidConnectionNotice = null;
      }, 5000);
    }
  }

  function handleDelete({ edges: deletedEdges }: { edges: Edge[] }) {
    for (const edge of deletedEdges) {
      if (edge.source.startsWith('input-') && edge.target.startsWith('room-')) {
        const devId = edge.source.replace('input-', '');
        const roomId = edge.target.replace('room-', '');
        roomStore.removeRoomInput(roomId, devId);
      } else if (edge.source.startsWith('room-') && edge.target.startsWith('output-')) {
        const roomId = edge.source.replace('room-', '');
        const devId = edge.target.replace('output-', '');
        roomStore.removeRoomOutput(roomId, devId);
      }
    }
  }

  function syncFlowGraph(forceResetPositions = false) {
    const inputDevs = deviceStore.inputDevices;
    const outputDevs = deviceStore.outputDevices;
    const currentRooms = roomStore.rooms;

    const roomSummaries = currentRooms.map((r) => ({ id: r.id, name: r.name }));

    const nextNodes: Node[] = [];
    const nextEdges: Edge[] = [];

    // 1. Generate Input Nodes (Left Column)
    inputDevs.forEach((dev, index) => {
      const nodeId = `input-${dev.id}`;
      let pos = forceResetPositions ? null : nodePositionCache.get(nodeId);
      if (!pos) {
        pos = getDefaultPosition('input', index);
        nodePositionCache.set(nodeId, pos);
      }

      const assignedRooms = currentRooms
        .filter((r) => r.inputs && r.inputs.includes(dev.id))
        .map((r) => ({ id: r.id, name: r.name }));

      nextNodes.push({
        id: nodeId,
        type: 'inputDevice',
        position: { x: pos.x, y: pos.y },
        data: {
          device: dev,
          assignedRooms,
          allRooms: roomSummaries,
          onAddToRoom: (roomId: string) => roomStore.addRoomInput(roomId, dev.id),
          onRemoveFromRoom: (roomId: string) => roomStore.removeRoomInput(roomId, dev.id),
        },
      });
    });

    // 2. Generate Room Nodes (Center Column)
    currentRooms.forEach((room, index) => {
      const nodeId = `room-${room.id}`;
      let pos = forceResetPositions ? null : nodePositionCache.get(nodeId);
      if (!pos) {
        pos = getDefaultPosition('room', index);
        nodePositionCache.set(nodeId, pos);
      }

      const assignedInputs = deviceStore.devices.filter(
        (d) => d.max_input > 0 && room.inputs && room.inputs.includes(d.id)
      );
      const assignedOutputs = deviceStore.devices.filter(
        (d) => d.max_output > 0 && room.outputs && room.outputs.includes(d.id)
      );

      const availableInputs = deviceStore.devices.filter(
        (d) => d.max_input > 0 && (!room.inputs || !room.inputs.includes(d.id))
      );
      const availableOutputs = deviceStore.devices.filter(
        (d) => d.max_output > 0 && (!room.outputs || !room.outputs.includes(d.id))
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
          onAddInput: (devId: string) => roomStore.addRoomInput(room.id, devId),
          onRemoveInput: (devId: string) => roomStore.removeRoomInput(room.id, devId),
          onAddOutput: (devId: string) => roomStore.addRoomOutput(room.id, devId),
          onRemoveOutput: (devId: string) => roomStore.removeRoomOutput(room.id, devId),
          onDeleteRoom: () => {
            if (confirm(`Delete audio room "${room.name}" (${room.id})?`)) {
              roomStore.deleteRoom(room.id);
            }
          },
        },
      });

      // 3. Generate Edges for this Room
      // (a) Input -> Room
      if (room.inputs) {
        for (const inputDevId of room.inputs) {
          if (inputDevs.some((d) => d.id === inputDevId)) {
            nextEdges.push({
              id: `edge-in-${inputDevId}-to-${room.id}`,
              source: `input-${inputDevId}`,
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
          if (outputDevs.some((d) => d.id === outputDevId)) {
            nextEdges.push({
              id: `edge-room-${room.id}-to-out-${outputDevId}`,
              source: `room-${room.id}`,
              target: `output-${outputDevId}`,
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

    // 4. Generate Output Nodes (Right Column)
    outputDevs.forEach((dev, index) => {
      const nodeId = `output-${dev.id}`;
      let pos = forceResetPositions ? null : nodePositionCache.get(nodeId);
      if (!pos) {
        pos = getDefaultPosition('output', index);
        nodePositionCache.set(nodeId, pos);
      }

      const sourceRooms = currentRooms
        .filter((r) => r.outputs && r.outputs.includes(dev.id))
        .map((r) => ({ id: r.id, name: r.name }));

      nextNodes.push({
        id: nodeId,
        type: 'outputDevice',
        position: { x: pos.x, y: pos.y },
        data: {
          device: dev,
          sourceRooms,
          allRooms: roomSummaries,
          onAddToRoom: (roomId: string) => roomStore.addRoomOutput(roomId, dev.id),
          onRemoveFromRoom: (roomId: string) => roomStore.removeRoomOutput(roomId, dev.id),
        },
      });
    });

    nodes = nextNodes;
    edges = nextEdges;
  }

  // Reactive effect: Rebuild graph whenever rooms or devices update
  $effect(() => {
    // Read reactive values to register dependency
    const _rooms = roomStore.rooms;
    const _devices = deviceStore.devices;
    untrack(() => {
      syncFlowGraph(false);
    });
  });

  const totalSummingPaths = $derived(
    roomStore.rooms.reduce((acc, r) => acc + (r.inputs?.length || 0) * (r.outputs?.length || 0), 0)
  );
</script>

<div class="relative h-full w-full bg-studio-dark select-none overflow-hidden">
  <!-- Prominent Sticky Error Notification Banner -->
  {#if roomStore.lastError || deviceStore.error || invalidConnectionNotice}
    <div
      class="absolute top-2 left-1/2 -translate-x-1/2 z-50 w-11/12 max-w-2xl rounded-xl bg-red-950/95 border border-red-500/50 p-3 shadow-2xl backdrop-blur-md flex items-center justify-between gap-3 text-xs text-red-200 animate-in fade-in slide-in-from-top-2 duration-200"
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
    {nodeTypes}
    fitView
    minZoom={0.2}
    maxZoom={2.0}
    onnodedragstop={handleNodeDragStop}
    onconnect={handleConnect}
    ondelete={handleDelete}
    {isValidConnection}
    class="bg-zinc-950"
  >
    <!-- Background grid dots -->
    <Background gap={20} size={1.5} color="#27272a" />

    <!-- Canvas Navigation Controls -->
    <Controls class="!bg-zinc-900/90 !border !border-zinc-800 !text-zinc-200 !rounded-xl !shadow-xl overflow-hidden" />

    <!-- MiniMap for large studio configurations -->
    <MiniMap
      nodeStrokeColor="#06b6d4"
      nodeColor="#18181b"
      maskColor="rgba(9, 9, 11, 0.8)"
      class="!bg-zinc-950 !border !border-zinc-800/90 !rounded-xl !shadow-2xl overflow-hidden"
    />

    <!-- Top Left Information & Paradigm Panel -->
    <Panel position="top-left" class="m-3">
      <div class="rounded-2xl border border-zinc-800/90 bg-zinc-950/85 backdrop-blur-md p-3.5 shadow-2xl flex flex-col gap-1.5 max-w-sm">
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
              {roomStore.rooms.length} Rooms &bull; {deviceStore.inputDevices.length} In &bull; {deviceStore.outputDevices.length} Out &bull; <strong class="text-cyan-300 font-bold">{totalSummingPaths} Active Paths</strong>
            </p>
          </div>
        </div>
        <div class="rounded-lg bg-zinc-900/60 p-2 text-[10px] font-sans text-zinc-400 leading-snug border border-zinc-800/80">
          <span class="text-cyan-300 font-semibold font-mono">Routing Rule:</span>
          Inputs connect into a Room. Within that Room, every input device plays to every output device. Drag wires between nodes or use "+ Room" chips.
        </div>
      </div>
    </Panel>

    <!-- Top Right Action Toolbar Panel -->
    <Panel position="top-right" class="m-3">
      <div class="flex items-center gap-2 rounded-2xl border border-zinc-800/90 bg-zinc-950/85 backdrop-blur-md p-2 shadow-2xl">
        <!-- Add Room Button -->
        <button
          type="button"
          onclick={() => (isCreateRoomOpen = true)}
          class="inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-3 py-1.5 text-xs font-mono font-bold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 transition-all"
        >
          <Plus class="w-3.5 h-3.5" />
          <span>New Room</span>
        </button>

        <!-- Add Virtual Device Button -->
        <button
          type="button"
          onclick={() => (isCreateVirtualOpen = true)}
          class="inline-flex items-center gap-1.5 rounded-lg border border-purple-500/30 bg-purple-500/10 px-3 py-1.5 text-xs font-mono font-semibold text-purple-300 hover:bg-purple-500/20 transition-all"
        >
          <Radio class="w-3.5 h-3.5 text-purple-400" />
          <span>New Virtual Loopback</span>
        </button>

        <!-- Auto-Arrange Layout Button -->
        <button
          type="button"
          onclick={handleAutoArrange}
          class="inline-flex items-center gap-1.5 rounded-lg border border-zinc-700 bg-zinc-900 px-2.5 py-1.5 text-xs font-mono font-medium text-zinc-300 hover:bg-zinc-800 transition-colors"
          title="Reset layout and align nodes in Left-to-Right signal flow columns"
        >
          <RotateCcw class="w-3.5 h-3.5 text-cyan-400" />
          <span class="hidden sm:inline">Auto-Arrange</span>
        </button>

        <!-- Simulate Metering Toggle -->
        <button
          type="button"
          onclick={() => meterStore.toggleSimulation()}
          class="inline-flex items-center gap-1 rounded-lg border px-2.5 py-1.5 text-xs font-mono transition-colors {meterStore.useSimulation
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
          }}
          class="inline-flex items-center justify-center h-8 w-8 rounded-lg border border-zinc-800 bg-zinc-900 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-800 transition-colors"
          title="Reload devices and rooms"
          aria-label="Reload devices and rooms"
        >
          <RefreshCw class="w-3.5 h-3.5 {roomStore.isLoading ? 'animate-spin text-cyan-400' : ''}" />
        </button>
      </div>
    </Panel>
  </SvelteFlow>

  <!-- Modals -->
  <CreateRoomModal bind:isOpen={isCreateRoomOpen} />
  <CreateVirtualModal bind:isOpen={isCreateVirtualOpen} />
</div>
