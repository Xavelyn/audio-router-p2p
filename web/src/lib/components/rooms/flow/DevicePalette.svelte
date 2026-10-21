<script lang="ts">
  import { clusterStore } from '$lib/stores/cluster.svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import { flowLayoutStore } from '$lib/stores/flowLayout.svelte';
  import type { AudioDevice, AudioDeviceSummary, ClusterNodeSummary } from '$lib/api/types';
  import Search from '@lucide/svelte/icons/search';
  import X from '@lucide/svelte/icons/x';
  import Plus from '@lucide/svelte/icons/plus';
  import Check from '@lucide/svelte/icons/check';
  import Mic from '@lucide/svelte/icons/mic';
  import Speaker from '@lucide/svelte/icons/speaker';
  import Radio from '@lucide/svelte/icons/radio';
  import Layers from '@lucide/svelte/icons/layers';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import PanelLeftClose from '@lucide/svelte/icons/panel-left-close';
  import GripVertical from '@lucide/svelte/icons/grip-vertical';
  import Server from '@lucide/svelte/icons/server';

  type DirectionFilter = 'all' | 'in' | 'out' | 'virt';

  interface Props {
    onAddDevice?: (device: AudioDevice | AudioDeviceSummary, direction?: 'input' | 'output') => void;
    onFocusDevice?: (deviceId: string) => void;
  }

  let { onAddDevice, onFocusDevice }: Props = $props();

  let searchQuery = $state('');
  let directionFilter = $state<DirectionFilter>('all');

  interface GroupedNode {
    id: string;
    name: string;
    address: string;
    isLocal: boolean;
    swimState: string;
    healthScore: number;
    clockRole?: string;
    devices: (AudioDevice | AudioDeviceSummary)[];
  }

  // Derive consolidated list of nodes and their audio devices
  const groupedNodes = $derived.by<GroupedNode[]>(() => {
    const result: GroupedNode[] = [];
    const clusterNodes = clusterStore.nodes;
    const localDevices = deviceStore.devices;

    if (clusterNodes.length > 0) {
      // Find local node first
      const localNode = clusterNodes.find((n) => n.is_local) || clusterNodes[0];
      const remoteNodes = clusterNodes.filter((n) => n !== localNode);

      // Local node with merged devices
      if (localNode) {
        const localDevMap = new Map<string, AudioDevice | AudioDeviceSummary>();
        // Add cluster-reported devices
        if (localNode.devices) {
          for (const d of localNode.devices) localDevMap.set(d.id, d);
        }
        // Ensure local deviceStore devices are present
        for (const d of localDevices) {
          localDevMap.set(d.id, d);
        }

        result.push({
          id: localNode.node_id || localNode.id,
          name: localNode.name || 'Local Gateway Node',
          address: localNode.api_addr || '127.0.0.1:8080',
          isLocal: true,
          swimState: localNode.swim_state || 'alive',
          healthScore: localNode.health_score ?? 100,
          clockRole: localNode.clock_role,
          devices: Array.from(localDevMap.values()),
        });
      }

      // Remote nodes sorted by name
      for (const node of remoteNodes) {
        result.push({
          id: node.node_id || node.id,
          name: node.name,
          address: node.api_addr,
          isLocal: false,
          swimState: node.swim_state || 'alive',
          healthScore: node.health_score ?? 90,
          clockRole: node.clock_role,
          devices: node.devices || [],
        });
      }
    } else {
      // Fallback: only local deviceStore available
      result.push({
        id: 'node-local',
        name: 'Local Gateway Node',
        address: '127.0.0.1:8080',
        isLocal: true,
        swimState: 'alive',
        healthScore: 100,
        clockRole: 'master',
        devices: localDevices,
      });
    }

    return result;
  });

  // Filter devices according to search text and direction
  function filterDevice(dev: AudioDevice | AudioDeviceSummary, nodeName: string): boolean {
    // Text search
    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase().trim();
      const matchName = dev.name.toLowerCase().includes(q);
      const matchId = dev.id.toLowerCase().includes(q);
      const matchNode = nodeName.toLowerCase().includes(q);
      if (!matchName && !matchId && !matchNode) return false;
    }

    // Direction filter
    if (directionFilter === 'in') {
      return dev.max_input > 0;
    }
    if (directionFilter === 'out') {
      return dev.max_output > 0;
    }
    if (directionFilter === 'virt') {
      return dev.is_virtual;
    }

    return true;
  }

  // Drag start handler
  function handleDragStart(
    event: DragEvent,
    device: AudioDevice | AudioDeviceSummary,
    nodeId: string
  ) {
    if (!event.dataTransfer) return;

    const direction =
      device.max_input > 0 && device.max_output === 0
        ? 'input'
        : device.max_output > 0 && device.max_input === 0
          ? 'output'
          : 'duplex';

    const payload = {
      deviceId: device.id,
      nodeId,
      direction,
      name: device.name,
      isVirtual: device.is_virtual,
    };

    event.dataTransfer.setData('application/json', JSON.stringify(payload));
    event.dataTransfer.effectAllowed = 'copy';
  }

  // Total cluster device count
  const totalClusterDevices = $derived(
    groupedNodes.reduce((acc, n) => acc + n.devices.length, 0)
  );

  const stagedClusterDevices = $derived(
    groupedNodes.reduce((acc, n) => {
      return acc + n.devices.filter((d) => flowLayoutStore.isDeviceEnabled(d.id)).length;
    }, 0)
  );
</script>

<aside
  class="w-80 h-full flex flex-col bg-zinc-950/95 border-r border-zinc-800/80 backdrop-blur-md select-none shrink-0 z-30 transition-all duration-200"
  aria-label="Audio Device Staging Palette"
>
  <!-- Palette Header -->
  <div class="p-3.5 border-b border-zinc-800/80 flex items-center justify-between shrink-0">
    <div class="flex items-center gap-2">
      <div class="flex h-7 w-7 items-center justify-center rounded-lg bg-cyan-500/10 border border-cyan-500/30 text-cyan-400">
        <Layers class="w-4 h-4" />
      </div>
      <div>
        <h2 class="text-xs font-mono font-bold tracking-wider uppercase text-zinc-100 flex items-center gap-1.5">
          <span>Device Palette</span>
          <span class="rounded bg-cyan-500/20 px-1 py-0.2 text-[9px] text-cyan-300 font-semibold border border-cyan-500/30">
            STAGING
          </span>
        </h2>
        <p class="text-[10px] font-mono text-zinc-400">
          {stagedClusterDevices} / {totalClusterDevices} staged on canvas
        </p>
      </div>
    </div>

    <!-- Collapse Palette Button -->
    <button
      type="button"
      onclick={() => flowLayoutStore.setSidebarOpen(false)}
      class="rounded-lg p-1.5 text-zinc-400 hover:text-zinc-100 hover:bg-zinc-800/80 transition-colors"
      title="Collapse Device Palette"
      aria-label="Collapse Device Palette"
    >
      <PanelLeftClose class="w-4 h-4" />
    </button>
  </div>

  <!-- Search & Direction Filter Chips -->
  <div class="p-3 border-b border-zinc-800/80 space-y-2 shrink-0 bg-zinc-950/50">
    <!-- Search Bar -->
    <div class="relative">
      <Search class="absolute left-2.5 top-1/2 -translate-y-1/2 w-3.5 h-3.5 text-zinc-500 pointer-events-none" />
      <input
        type="text"
        placeholder="Filter devices or nodes..."
        bind:value={searchQuery}
        class="w-full bg-zinc-900/90 border border-zinc-800 rounded-lg pl-8 pr-7 py-1.5 text-xs text-zinc-200 placeholder-zinc-500 focus:outline-none focus:border-cyan-500/50 focus:ring-1 focus:ring-cyan-500/30 font-mono transition-all"
      />
      {#if searchQuery}
        <button
          type="button"
          onclick={() => (searchQuery = '')}
          class="absolute right-2 top-1/2 -translate-y-1/2 text-zinc-500 hover:text-zinc-300"
          aria-label="Clear search"
        >
          <X class="w-3.5 h-3.5" />
        </button>
      {/if}
    </div>

    <!-- Direction Filter Pills -->
    <div class="flex items-center gap-1">
      <button
        type="button"
        onclick={() => (directionFilter = 'all')}
        class="flex-1 text-[10px] font-mono py-1 rounded border transition-all text-center {directionFilter === 'all'
          ? 'bg-cyan-500 text-zinc-950 font-bold border-cyan-400 shadow-sm shadow-cyan-500/30'
          : 'bg-zinc-900 border-zinc-800 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-850'}"
      >
        All
      </button>
      <button
        type="button"
        onclick={() => (directionFilter = 'in')}
        class="flex-1 text-[10px] font-mono py-1 rounded border transition-all text-center {directionFilter === 'in'
          ? 'bg-cyan-500 text-zinc-950 font-bold border-cyan-400 shadow-sm shadow-cyan-500/30'
          : 'bg-zinc-900 border-zinc-800 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-850'}"
      >
        Inputs
      </button>
      <button
        type="button"
        onclick={() => (directionFilter = 'out')}
        class="flex-1 text-[10px] font-mono py-1 rounded border transition-all text-center {directionFilter === 'out'
          ? 'bg-cyan-500 text-zinc-950 font-bold border-cyan-400 shadow-sm shadow-cyan-500/30'
          : 'bg-zinc-900 border-zinc-800 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-850'}"
      >
        Outputs
      </button>
      <button
        type="button"
        onclick={() => (directionFilter = 'virt')}
        class="flex-1 text-[10px] font-mono py-1 rounded border transition-all text-center {directionFilter === 'virt'
          ? 'bg-cyan-500 text-zinc-950 font-bold border-cyan-400 shadow-sm shadow-cyan-500/30'
          : 'bg-zinc-900 border-zinc-800 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-850'}"
      >
        Virtual
      </button>
    </div>
  </div>

  <!-- Node Accordions List -->
  <div class="flex-1 overflow-y-auto p-3 space-y-3">
    {#each groupedNodes as node (node.id)}
      {@const isCollapsed = flowLayoutStore.isNodeCollapsed(node.id)}
      {@const matchingDevices = node.devices.filter((d) => filterDevice(d, node.name))}

      <div
        class="rounded-xl border transition-all overflow-hidden {node.isLocal
          ? 'border-cyan-500/30 bg-cyan-950/10 shadow-sm shadow-cyan-500/5'
          : 'border-zinc-800/90 bg-zinc-900/40 hover:border-zinc-700'}"
      >
        <!-- Accordion Header -->
        <button
          type="button"
          onclick={() => flowLayoutStore.toggleNodeCollapsed(node.id)}
          class="w-full text-left p-2.5 flex items-center justify-between gap-2 hover:bg-zinc-800/40 transition-colors"
          aria-expanded={!isCollapsed}
        >
          <div class="flex items-center gap-2 min-w-0">
            <span class="text-zinc-400 shrink-0">
              {#if isCollapsed}
                <ChevronRight class="w-3.5 h-3.5" />
              {:else}
                <ChevronDown class="w-3.5 h-3.5" />
              {/if}
            </span>

            <div class="min-w-0">
              <div class="flex items-center gap-1.5 leading-tight">
                <span class="text-xs font-bold text-zinc-100 truncate font-mono">
                  {node.name}
                </span>
                {#if node.isLocal}
                  <span class="rounded bg-cyan-500/20 px-1 py-0.2 text-[8px] font-mono font-bold text-cyan-300 border border-cyan-500/40 shrink-0">
                    LOCAL
                  </span>
                {/if}
              </div>
              <div class="flex items-center gap-1.5 text-[9px] font-mono text-zinc-400 truncate">
                <span class="h-1.5 w-1.5 rounded-full {node.swimState === 'alive' ? 'bg-emerald-400 shadow-[0_0_4px_rgba(52,211,153,0.8)]' : 'bg-amber-400'}"></span>
                <span>{node.address.replace(/^https?:\/\//, '')}</span>
                {#if node.clockRole}
                  <span class="text-zinc-500">&bull;</span>
                  <span class="uppercase text-cyan-400">{node.clockRole}</span>
                {/if}
              </div>
            </div>
          </div>

          <!-- Device count badge -->
          <div class="shrink-0 text-right">
            <span class="rounded bg-zinc-800/80 px-1.5 py-0.5 text-[9px] font-mono text-zinc-300 border border-zinc-700/60">
              {matchingDevices.length} {matchingDevices.length === 1 ? 'dev' : 'devs'}
            </span>
          </div>
        </button>

        <!-- Accordion Body: Device Cards -->
        {#if !isCollapsed}
          <div class="p-2 pt-0 space-y-1.5 border-t border-zinc-800/60" role="list">
            {#if matchingDevices.length === 0}
              <div class="p-3 text-center text-[10px] font-mono text-zinc-500 italic">
                No matching devices found
              </div>
            {:else}
              {#each matchingDevices as device (device.id)}
                {@const isStaged = flowLayoutStore.isDeviceEnabled(device.id)}
                {@const isInput = device.max_input > 0 && device.max_output === 0}
                {@const isOutput = device.max_output > 0 && device.max_input === 0}
                {@const isDuplex = device.max_input > 0 && device.max_output > 0}

                <!-- Draggable Device Card -->
                <div
                  role="listitem"
                  draggable="true"
                  ondragstart={(e) => handleDragStart(e, device, node.id)}
                  aria-label="Draggable device {device.name}"
                  class="group relative rounded-lg border p-2 bg-zinc-900/90 transition-all flex items-center justify-between gap-2 cursor-grab active:cursor-grabbing {isStaged
                    ? 'border-cyan-500/40 bg-cyan-950/15 shadow-sm'
                    : 'border-zinc-800/90 hover:border-cyan-500/50 hover:bg-zinc-850/90'}"
                  title="Drag onto Canvas or click + Add to route"
                >
                  <!-- Drag Handle Icon & Device Info -->
                  <div class="flex items-center gap-1.5 min-w-0">
                    <GripVertical class="w-3.5 h-3.5 text-zinc-600 group-hover:text-cyan-400 shrink-0 transition-colors" />

                    <div class="flex h-6 w-6 items-center justify-center rounded bg-zinc-800/80 border border-zinc-700 text-zinc-300 shrink-0">
                      {#if device.is_virtual}
                        <Radio class="w-3 h-3 text-purple-400" />
                      {:else if isInput}
                        <Mic class="w-3 h-3 text-cyan-400" />
                      {:else if isOutput}
                        <Speaker class="w-3 h-3 text-purple-400" />
                      {:else}
                        <Layers class="w-3 h-3 text-emerald-400" />
                      {/if}
                    </div>

                    <div class="min-w-0">
                      <div class="text-xs font-semibold text-zinc-200 truncate leading-snug font-sans group-hover:text-white" title={device.name}>
                        {device.name}
                      </div>
                      <div class="flex items-center gap-1 text-[9px] font-mono text-zinc-400">
                        <!-- Direction Badge -->
                        {#if isInput}
                          <span class="text-cyan-400 font-bold">IN ({device.max_input}ch)</span>
                        {:else if isOutput}
                          <span class="text-purple-400 font-bold">OUT ({device.max_output}ch)</span>
                        {:else}
                          <span class="text-emerald-400 font-bold">DUP ({device.max_input}x{device.max_output})</span>
                        {/if}

                        <span class="text-zinc-600">&bull;</span>
                        <span>{device.default_sample_rate / 1000}k</span>

                        <span class="text-zinc-600">&bull;</span>
                        <span class="{device.is_virtual ? 'text-purple-400' : 'text-zinc-400'}">
                          {device.is_virtual ? 'VIRT' : 'HW'}
                        </span>
                      </div>
                    </div>
                  </div>

                  <!-- Staging Status Pill / Add Button -->
                  <div class="shrink-0 flex items-center gap-1">
                    {#if isStaged}
                      <button
                        type="button"
                        onclick={() => onFocusDevice?.(device.id)}
                        class="inline-flex items-center gap-1 rounded bg-cyan-500/20 px-2 py-0.5 text-[9px] font-mono font-bold text-cyan-300 border border-cyan-500/40 hover:bg-cyan-500/30 transition-colors"
                        title="Already staged on canvas. Click to focus."
                      >
                        <Check class="w-2.5 h-2.5 text-cyan-400" />
                        <span>On Canvas</span>
                      </button>
                    {:else}
                      <button
                        type="button"
                        onclick={() => onAddDevice?.(device, isOutput ? 'output' : 'input')}
                        class="inline-flex items-center gap-0.5 rounded bg-cyan-500/10 hover:bg-cyan-500 px-2 py-0.5 text-[9px] font-mono font-bold text-cyan-300 hover:text-zinc-950 border border-cyan-500/30 transition-all shadow-sm"
                        title="Add to canvas"
                        aria-label="Add {device.name} to canvas"
                      >
                        <Plus class="w-2.5 h-2.5" />
                        <span>Add</span>
                      </button>
                    {/if}
                  </div>
                </div>
              {/each}
            {/if}
          </div>
        {/if}
      </div>
    {/each}
  </div>

  <!-- Palette Footer Stats & Hint -->
  <div class="p-3 border-t border-zinc-800/80 bg-zinc-950/70 shrink-0 text-[10px] font-mono text-zinc-400 flex items-center justify-between">
    <div class="flex items-center gap-1.5">
      <Server class="w-3.5 h-3.5 text-cyan-400" />
      <span>{groupedNodes.length} {groupedNodes.length === 1 ? 'Node' : 'Nodes'}</span>
    </div>
    <span class="text-zinc-500">Drag card to canvas &rarr;</span>
  </div>
</aside>
