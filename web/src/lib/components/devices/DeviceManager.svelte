<script lang="ts">
  import { onMount } from 'svelte';
  import { clusterStore } from '$lib/stores/cluster.svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import type { AudioDeviceSummary, CreateVirtualDeviceRequest } from '$lib/api/types';

  import Speaker from '@lucide/svelte/icons/speaker';
  import Mic from '@lucide/svelte/icons/mic';
  import Plus from '@lucide/svelte/icons/plus';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import VolumeX from '@lucide/svelte/icons/volume-x';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Layers from '@lucide/svelte/icons/layers';
  import Server from '@lucide/svelte/icons/server';
  import Filter from '@lucide/svelte/icons/filter';
  import X from '@lucide/svelte/icons/x';
  import ArrowDownLeft from '@lucide/svelte/icons/arrow-down-left';
  import ArrowUpRight from '@lucide/svelte/icons/arrow-up-right';

  let isModalOpen = $state(false);

  // Form state for creating virtual device
  let newName = $state('Loopback-Bus-A');
  let newDirection = $state<'input' | 'output' | 'duplex'>('duplex');
  let newChannels = $state<1 | 2 | 4 | 6 | 8>(2);
  let newSampleRate = $state<44100 | 48000 | 96000>(48000);
  let isSubmitting = $state(false);
  let formError = $state<string | null>(null);

  // Active filters
  let selectedNodeFilter = $state<string>('all');
  let selectedKindFilter = $state<'all' | 'inputs' | 'outputs' | 'virtual'>('all');

  onMount(() => {
    clusterStore.startPolling(2500);
    deviceStore.loadDevices();
  });

  // Categorized & filtered devices
  const allDevices = $derived(clusterStore.devices.length > 0 ? clusterStore.devices : deviceStore.devices.map(d => ({
    id: d.id,
    device_id: d.id,
    node_id: 'local',
    node_name: 'Local Node',
    name: d.name,
    direction: d.max_input > 0 && d.max_output > 0 ? 'duplex' : d.max_input > 0 ? 'input' : 'output',
    max_input: d.max_input,
    max_output: d.max_output,
    default_sample_rate: d.default_sample_rate,
    is_virtual: d.is_virtual,
    volume: d.volume ?? 1.0,
    muted: d.muted ?? false,
    supported_layouts: d.supported_layouts,
    state: 'active',
  })));

  const filteredByNode = $derived.by<AudioDeviceSummary[]>(() => {
    if (selectedNodeFilter === 'all') return allDevices;
    if (selectedNodeFilter === 'local') {
      const localId = clusterStore.localNode?.id || clusterStore.localNode?.node_id || 'local';
      return allDevices.filter((d) => d.node_id === localId || d.node_name?.toLowerCase().includes('local'));
    }
    return allDevices.filter((d) => d.node_id === selectedNodeFilter);
  });

  const inputsList = $derived(
    filteredByNode.filter((d) => !d.is_virtual && (d.direction === 'input' || d.direction === 'duplex' || d.max_input > 0))
  );

  const outputsList = $derived(
    filteredByNode.filter((d) => !d.is_virtual && (d.direction === 'output' || d.direction === 'duplex' || d.max_output > 0))
  );

  const virtualList = $derived(
    filteredByNode.filter((d) => d.is_virtual)
  );

  // Node lookup helper
  function getNodeInfo(nodeId: string) {
    const node = clusterStore.nodes.find((n) => n.id === nodeId || n.node_id === nodeId);
    if (!node) {
      return {
        name: nodeId === 'local' ? 'Local Gateway' : nodeId,
        isLocal: nodeId === 'local',
        swimState: 'alive',
      };
    }
    return {
      name: node.name || node.id,
      isLocal: node.is_local,
      swimState: node.swim_state || 'alive',
    };
  }

  async function handleCreateVirtual() {
    isSubmitting = true;
    formError = null;
    try {
      await deviceStore.createVirtual({
        name: newName,
        direction: newDirection,
        channels: newChannels,
        sample_rate: newSampleRate,
      });
      await clusterStore.pollCluster();
      isModalOpen = false;
      newName = 'Loopback-Bus-' + Math.floor(Math.random() * 100);
    } catch (err: any) {
      formError = err.message || 'Failed to create virtual audio interface';
    } finally {
      isSubmitting = false;
    }
  }

  async function handleDeleteVirtual(deviceId: string) {
    if (confirm('Delete this virtual audio loopback interface?')) {
      await deviceStore.deleteVirtual(deviceId);
      await clusterStore.pollCluster();
    }
  }

  function handleVolumeChange(dev: AudioDeviceSummary, val: number) {
    clusterStore.patchDevice(dev.node_id, dev.device_id || dev.id, { volume: val });
  }

  function handleMuteToggle(dev: AudioDeviceSummary) {
    clusterStore.patchDevice(dev.node_id, dev.device_id || dev.id, { muted: !dev.muted });
  }
</script>

<div class="flex flex-col h-full bg-studio-dark select-none overflow-hidden">
  <!-- Toolbar -->
  <div class="flex items-center justify-between border-b border-zinc-800 bg-zinc-950/80 px-4 py-2.5 backdrop-blur shrink-0">
    <div class="flex items-center gap-3">
      <div class="flex items-center gap-2">
        <Speaker class="w-4 h-4 text-cyan-400" />
        <h1 class="text-sm font-semibold tracking-wide text-zinc-100 uppercase">
          Cluster Audio Device Registry
        </h1>
      </div>
      <span class="rounded bg-zinc-800 px-2 py-0.5 text-xs font-mono text-zinc-300">
        {allDevices.length} Total Devices across {clusterStore.nodes.length || 1} Nodes
      </span>
    </div>

    <!-- Actions -->
    <div class="flex items-center gap-2">
      <button
        onclick={() => (isModalOpen = true)}
        class="inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-3 py-1.5 text-xs font-semibold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 transition-colors"
      >
        <Plus class="w-3.5 h-3.5" />
        <span>Provision Virtual Device</span>
      </button>

      <button
        onclick={() => { clusterStore.pollCluster(); deviceStore.loadDevices(); }}
        class="inline-flex items-center justify-center h-8 w-8 rounded-md border border-zinc-800 bg-zinc-900 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-800 transition-colors"
        title="Refresh Cluster Devices"
      >
        <RefreshCw class="w-3.5 h-3.5 {clusterStore.isLoading ? 'animate-spin text-cyan-400' : ''}" />
      </button>
    </div>
  </div>

  <!-- Multi-Node Filter Bar -->
  <div class="border-b border-zinc-800/80 bg-zinc-950/50 px-4 py-2 flex flex-wrap items-center justify-between gap-3 shrink-0">
    <!-- Node Filter Pills -->
    <div class="flex items-center gap-1.5 overflow-x-auto">
      <span class="text-xs font-mono text-zinc-500 flex items-center gap-1 mr-1">
        <Server class="w-3.5 h-3.5 text-zinc-400" />
        <span>Node:</span>
      </span>

      <!-- All Nodes -->
      <button
        onclick={() => (selectedNodeFilter = 'all')}
        class="rounded-lg px-2.5 py-1 text-xs font-mono transition-all {selectedNodeFilter === 'all'
          ? 'bg-cyan-500 text-zinc-950 font-bold shadow-sm shadow-cyan-500/20'
          : 'bg-zinc-900 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-200 border border-zinc-800'}"
      >
        All Nodes ({allDevices.length})
      </button>

      <!-- Local Node -->
      {#if clusterStore.localNode}
        {@const localId = clusterStore.localNode.node_id || clusterStore.localNode.id}
        {@const localDevCount = allDevices.filter(d => d.node_id === localId || d.node_name?.toLowerCase().includes('local')).length}
        <button
          onclick={() => (selectedNodeFilter = 'local')}
          class="rounded-lg px-2.5 py-1 text-xs font-mono transition-all flex items-center gap-1.5 {selectedNodeFilter === 'local'
            ? 'bg-cyan-500 text-zinc-950 font-bold shadow-sm shadow-cyan-500/20'
            : 'bg-zinc-900 text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100 border border-zinc-800'}"
        >
          <span class="h-1.5 w-1.5 rounded-full bg-cyan-400"></span>
          <span>Local Node ({localDevCount})</span>
        </button>
      {/if}

      <!-- Remote Nodes -->
      {#each clusterStore.remoteNodes as rNode}
        {@const rId = rNode.node_id || rNode.id}
        {@const rDevCount = allDevices.filter(d => d.node_id === rId).length}
        <button
          onclick={() => (selectedNodeFilter = rId)}
          class="rounded-lg px-2.5 py-1 text-xs font-mono transition-all flex items-center gap-1.5 {selectedNodeFilter === rId
            ? 'bg-cyan-500 text-zinc-950 font-bold shadow-sm shadow-cyan-500/20'
            : 'bg-zinc-900 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-200 border border-zinc-800'}"
        >
          <span class="h-1.5 w-1.5 rounded-full {rNode.swim_state === 'alive' ? 'bg-emerald-400' : 'bg-amber-400'}"></span>
          <span>{rNode.name || rId} ({rDevCount})</span>
        </button>
      {/each}
    </div>

    <!-- Kind Filter Pills -->
    <div class="flex items-center gap-1">
      <span class="text-xs font-mono text-zinc-500 flex items-center gap-1 mr-1">
        <Filter class="w-3.5 h-3.5 text-zinc-400" />
        <span>Kind:</span>
      </span>

      {#each [
        { id: 'all', label: 'All' },
        { id: 'inputs', label: 'Inputs' },
        { id: 'outputs', label: 'Outputs' },
        { id: 'virtual', label: 'Virtual' }
      ] as kind}
        <button
          onclick={() => (selectedKindFilter = kind.id as any)}
          class="rounded-md px-2 py-0.5 text-xs font-mono transition-colors {selectedKindFilter === kind.id
            ? 'bg-zinc-800 text-cyan-400 font-semibold border border-cyan-500/30'
            : 'text-zinc-400 hover:text-zinc-200'}"
        >
          {kind.label}
        </button>
      {/each}
    </div>
  </div>

  <!-- Device Cards Content Grid -->
  <div class="flex-1 overflow-y-auto p-4 md:p-6 bg-zinc-950 space-y-6">
    <div class="max-w-7xl mx-auto space-y-8">

      <!-- ==================================================================== -->
      <!-- 1. AUDIO INPUTS SECTION                                             -->
      <!-- ==================================================================== -->
      {#if selectedKindFilter === 'all' || selectedKindFilter === 'inputs'}
        <div>
          <div class="flex items-center justify-between mb-3 border-b border-zinc-800/80 pb-2">
            <div class="flex items-center gap-2">
              <div class="flex h-6 w-6 items-center justify-center rounded-md bg-blue-500/10 border border-blue-500/30 text-blue-400">
                <Mic class="w-3.5 h-3.5" />
              </div>
              <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
                Audio Inputs (Microphones, Line Ins, Stream Ingest) &bull; {inputsList.length} Endpoints
              </h2>
            </div>
            <span class="text-[10px] font-mono text-blue-400">SOURCES</span>
          </div>

          {#if inputsList.length === 0}
            <div class="rounded-xl border border-dashed border-zinc-800/80 p-6 text-center text-xs font-mono text-zinc-500 bg-zinc-900/20">
              No audio inputs match the current node filter.
            </div>
          {:else}
            <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
              {#each inputsList as dev}
                {@const nodeInfo = getNodeInfo(dev.node_id)}
                <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg hover:border-zinc-700 transition-all flex flex-col justify-between">
                  <div>
                    <!-- Top Badges: Node Origin & Direction -->
                    <div class="flex items-start justify-between gap-2">
                      <div class="flex items-center gap-1.5 flex-wrap">
                        <!-- Host Node Badge -->
                        <span class="rounded px-2 py-0.5 text-[10px] font-mono font-bold border flex items-center gap-1 {nodeInfo.isLocal
                          ? 'bg-cyan-500/10 text-cyan-300 border-cyan-500/30'
                          : 'bg-zinc-800 text-zinc-300 border-zinc-700'}">
                          <span class="h-1.5 w-1.5 rounded-full {nodeInfo.swimState === 'alive' ? 'bg-emerald-400' : 'bg-amber-400'}"></span>
                          <span>{nodeInfo.name}</span>
                        </span>

                        <span class="rounded bg-blue-500/10 px-2 py-0.5 text-[10px] font-mono font-semibold text-blue-400 border border-blue-500/20">
                          INPUT
                        </span>

                        {#if dev.status === 'quarantined'}
                          <span class="rounded bg-amber-500/20 px-2 py-0.5 text-[10px] font-mono font-bold text-amber-300 border border-amber-500/40 animate-pulse">
                            GHOST (OFFLINE)
                          </span>
                        {/if}
                      </div>

                      <span class="rounded bg-zinc-950/80 px-2 py-0.5 text-[10px] font-mono text-zinc-400 border border-zinc-800">
                        {dev.max_input || 2} Ch
                      </span>
                    </div>

                    <!-- Device Name -->
                    <h3 class="mt-2.5 text-sm font-semibold text-zinc-100 line-clamp-2">
                      {dev.name}
                    </h3>
                    <p class="text-[10px] font-mono text-zinc-500 truncate mt-0.5">
                      ID: {dev.device_id || dev.id}
                    </p>

                    <!-- Technical Specs Grid -->
                    <div class="mt-3 grid grid-cols-2 gap-2 text-xs font-mono">
                      <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                        <span class="text-[10px] text-zinc-400 block">CHANNELS</span>
                        <span class="text-zinc-200 font-bold">{dev.max_input} In Channels</span>
                      </div>
                      <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                        <span class="text-[10px] text-zinc-400 block">SAMPLE RATE</span>
                        <span class="text-cyan-400 font-bold">{dev.default_sample_rate / 1000} kHz</span>
                      </div>
                    </div>
                  </div>

                  <!-- Fader & Remote Mute Controls -->
                  <div class="mt-4 pt-3 border-t border-zinc-800/80 flex items-center justify-between gap-3">
                    <div class="flex items-center gap-2 flex-1">
                      <Volume2 class="w-3.5 h-3.5 text-zinc-400 shrink-0" />
                      <input
                        type="range"
                        min="0"
                        max="1.5"
                        step="0.05"
                        value={dev.volume ?? 1.0}
                        onchange={(e) => handleVolumeChange(dev, parseFloat((e.target as HTMLInputElement).value))}
                        class="w-full accent-cyan-400 cursor-pointer h-1.5 bg-zinc-800 rounded-lg"
                      />
                      <span class="text-[10px] font-mono text-zinc-400 w-8 text-right">
                        {Math.round((dev.volume ?? 1.0) * 100)}%
                      </span>
                    </div>

                    <button
                      onclick={() => handleMuteToggle(dev)}
                      class="rounded p-1.5 transition-colors {dev.muted ? 'bg-red-950/60 text-red-400 border border-red-500/30' : 'bg-zinc-900 text-zinc-400 hover:text-zinc-100 border border-zinc-800'}"
                      title={dev.muted ? 'Unmute' : 'Mute'}
                    >
                      {#if dev.muted}
                        <VolumeX class="w-3.5 h-3.5" />
                      {:else}
                        <Volume2 class="w-3.5 h-3.5" />
                      {/if}
                    </button>
                  </div>
                </div>
              {/each}
            </div>
          {/if}
        </div>
      {/if}

      <!-- ==================================================================== -->
      <!-- 2. AUDIO OUTPUTS SECTION                                            -->
      <!-- ==================================================================== -->
      {#if selectedKindFilter === 'all' || selectedKindFilter === 'outputs'}
        <div>
          <div class="flex items-center justify-between mb-3 border-b border-zinc-800/80 pb-2">
            <div class="flex items-center gap-2">
              <div class="flex h-6 w-6 items-center justify-center rounded-md bg-purple-500/10 border border-purple-500/30 text-purple-400">
                <Speaker class="w-3.5 h-3.5" />
              </div>
              <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
                Audio Outputs (Speakers, Stage Monitors, Line Out) &bull; {outputsList.length} Endpoints
              </h2>
            </div>
            <span class="text-[10px] font-mono text-purple-400">SINKS</span>
          </div>

          {#if outputsList.length === 0}
            <div class="rounded-xl border border-dashed border-zinc-800/80 p-6 text-center text-xs font-mono text-zinc-500 bg-zinc-900/20">
              No audio outputs match the current node filter.
            </div>
          {:else}
            <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
              {#each outputsList as dev}
                {@const nodeInfo = getNodeInfo(dev.node_id)}
                <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg hover:border-zinc-700 transition-all flex flex-col justify-between">
                  <div>
                    <!-- Top Badges: Node Origin & Direction -->
                    <div class="flex items-start justify-between gap-2">
                      <div class="flex items-center gap-1.5 flex-wrap">
                        <!-- Host Node Badge -->
                        <span class="rounded px-2 py-0.5 text-[10px] font-mono font-bold border flex items-center gap-1 {nodeInfo.isLocal
                          ? 'bg-cyan-500/10 text-cyan-300 border-cyan-500/30'
                          : 'bg-zinc-800 text-zinc-300 border-zinc-700'}">
                          <span class="h-1.5 w-1.5 rounded-full {nodeInfo.swimState === 'alive' ? 'bg-emerald-400' : 'bg-amber-400'}"></span>
                          <span>{nodeInfo.name}</span>
                        </span>

                        <span class="rounded bg-purple-500/10 px-2 py-0.5 text-[10px] font-mono font-semibold text-purple-400 border border-purple-500/20">
                          OUTPUT
                        </span>

                        {#if dev.status === 'quarantined'}
                          <span class="rounded bg-amber-500/20 px-2 py-0.5 text-[10px] font-mono font-bold text-amber-300 border border-amber-500/40 animate-pulse">
                            GHOST (OFFLINE)
                          </span>
                        {/if}
                      </div>

                      <span class="rounded bg-zinc-950/80 px-2 py-0.5 text-[10px] font-mono text-zinc-400 border border-zinc-800">
                        {dev.max_output || 2} Ch
                      </span>
                    </div>

                    <!-- Device Name -->
                    <h3 class="mt-2.5 text-sm font-semibold text-zinc-100 line-clamp-2">
                      {dev.name}
                    </h3>
                    <p class="text-[10px] font-mono text-zinc-500 truncate mt-0.5">
                      ID: {dev.device_id || dev.id}
                    </p>

                    <!-- Technical Specs Grid -->
                    <div class="mt-3 grid grid-cols-2 gap-2 text-xs font-mono">
                      <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                        <span class="text-[10px] text-zinc-400 block">OUTPUTS</span>
                        <span class="text-zinc-200 font-bold">{dev.max_output} Out Channels</span>
                      </div>
                      <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                        <span class="text-[10px] text-zinc-400 block">SAMPLE RATE</span>
                        <span class="text-cyan-400 font-bold">{dev.default_sample_rate / 1000} kHz</span>
                      </div>
                    </div>
                  </div>

                  <!-- Fader & Remote Mute Controls -->
                  <div class="mt-4 pt-3 border-t border-zinc-800/80 flex items-center justify-between gap-3">
                    <div class="flex items-center gap-2 flex-1">
                      <Volume2 class="w-3.5 h-3.5 text-zinc-400 shrink-0" />
                      <input
                        type="range"
                        min="0"
                        max="1.5"
                        step="0.05"
                        value={dev.volume ?? 1.0}
                        onchange={(e) => handleVolumeChange(dev, parseFloat((e.target as HTMLInputElement).value))}
                        class="w-full accent-cyan-400 cursor-pointer h-1.5 bg-zinc-800 rounded-lg"
                      />
                      <span class="text-[10px] font-mono text-zinc-400 w-8 text-right">
                        {Math.round((dev.volume ?? 1.0) * 100)}%
                      </span>
                    </div>

                    <button
                      onclick={() => handleMuteToggle(dev)}
                      class="rounded p-1.5 transition-colors {dev.muted ? 'bg-red-950/60 text-red-400 border border-red-500/30' : 'bg-zinc-900 text-zinc-400 hover:text-zinc-100 border border-zinc-800'}"
                      title={dev.muted ? 'Unmute' : 'Mute'}
                    >
                      {#if dev.muted}
                        <VolumeX class="w-3.5 h-3.5" />
                      {:else}
                        <Volume2 class="w-3.5 h-3.5" />
                      {/if}
                    </button>
                  </div>
                </div>
              {/each}
            </div>
          {/if}
        </div>
      {/if}

      <!-- ==================================================================== -->
      <!-- 3. VIRTUAL LOOPBACK DEVICES SECTION                                 -->
      <!-- ==================================================================== -->
      {#if selectedKindFilter === 'all' || selectedKindFilter === 'virtual'}
        <div>
          <div class="flex items-center justify-between mb-3 border-b border-zinc-800/80 pb-2">
            <div class="flex items-center gap-2">
              <div class="flex h-6 w-6 items-center justify-center rounded-md bg-cyan-500/10 border border-cyan-500/30 text-cyan-400">
                <Layers class="w-3.5 h-3.5" />
              </div>
              <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
                Virtual Audio Loopback Interfaces &bull; {virtualList.length} Buses
              </h2>
            </div>
            <button
              onclick={() => (isModalOpen = true)}
              class="text-xs font-mono text-cyan-400 hover:underline"
            >
              + Provision Loopback
            </button>
          </div>

          {#if virtualList.length === 0}
            <div class="rounded-xl border border-dashed border-zinc-800/80 p-6 text-center text-xs font-mono text-zinc-500 bg-zinc-900/20">
              No virtual loopback interfaces provisioned.
            </div>
          {:else}
            <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
              {#each virtualList as vDev}
                {@const nodeInfo = getNodeInfo(vDev.node_id)}
                <div class="rounded-xl border border-cyan-900/40 bg-zinc-900/80 p-4 shadow-lg hover:border-cyan-700/60 transition-all flex flex-col justify-between">
                  <div>
                    <div class="flex items-start justify-between gap-2">
                      <div class="flex items-center gap-1.5 flex-wrap">
                        <span class="rounded px-2 py-0.5 text-[10px] font-mono font-bold border flex items-center gap-1 {nodeInfo.isLocal
                          ? 'bg-cyan-500/10 text-cyan-300 border-cyan-500/30'
                          : 'bg-zinc-800 text-zinc-300 border-zinc-700'}">
                          <span class="h-1.5 w-1.5 rounded-full {nodeInfo.swimState === 'alive' ? 'bg-emerald-400' : 'bg-amber-400'}"></span>
                          <span>{nodeInfo.name}</span>
                        </span>

                        <span class="rounded bg-cyan-500/10 px-2 py-0.5 text-[10px] font-mono text-cyan-400 border border-cyan-500/20">
                          VIRTUAL BUS
                        </span>
                      </div>

                      {#if nodeInfo.isLocal}
                        <button
                          onclick={() => handleDeleteVirtual(vDev.id || vDev.device_id || '')}
                          class="rounded p-1 text-zinc-500 hover:text-red-400 hover:bg-red-950/30 transition-colors"
                          title="Delete Virtual Device"
                        >
                          <Trash2 class="w-3.5 h-3.5" />
                        </button>
                      {/if}
                    </div>

                    <h3 class="mt-2.5 text-sm font-semibold text-zinc-100">
                      {vDev.name}
                    </h3>
                    <p class="text-[10px] font-mono text-zinc-500 truncate mt-0.5">
                      ID: {vDev.device_id || vDev.id}
                    </p>

                    <div class="mt-3 grid grid-cols-2 gap-2 text-xs font-mono">
                      <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                        <span class="text-[10px] text-zinc-400 block">CHANNELS</span>
                        <span class="text-zinc-200 font-bold">{Math.max(vDev.max_input, vDev.max_output)} Channels</span>
                      </div>
                      <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                        <span class="text-[10px] text-zinc-400 block">SAMPLE RATE</span>
                        <span class="text-cyan-400 font-bold">{vDev.default_sample_rate / 1000} kHz</span>
                      </div>
                    </div>
                  </div>

                  <!-- Fader & Mute -->
                  <div class="mt-4 pt-3 border-t border-zinc-800/80 flex items-center justify-between gap-3">
                    <div class="flex items-center gap-2 flex-1">
                      <Volume2 class="w-3.5 h-3.5 text-zinc-400 shrink-0" />
                      <input
                        type="range"
                        min="0"
                        max="1.5"
                        step="0.05"
                        value={vDev.volume ?? 1.0}
                        onchange={(e) => handleVolumeChange(vDev, parseFloat((e.target as HTMLInputElement).value))}
                        class="w-full accent-cyan-400 cursor-pointer h-1.5 bg-zinc-800 rounded-lg"
                      />
                      <span class="text-[10px] font-mono text-zinc-400 w-8 text-right">
                        {Math.round((vDev.volume ?? 1.0) * 100)}%
                      </span>
                    </div>

                    <button
                      onclick={() => handleMuteToggle(vDev)}
                      class="rounded p-1.5 transition-colors {vDev.muted ? 'bg-red-950/60 text-red-400 border border-red-500/30' : 'bg-zinc-900 text-zinc-400 hover:text-zinc-100 border border-zinc-800'}"
                      title={vDev.muted ? 'Unmute' : 'Mute'}
                    >
                      {#if vDev.muted}
                        <VolumeX class="w-3.5 h-3.5" />
                      {:else}
                        <Volume2 class="w-3.5 h-3.5" />
                      {/if}
                    </button>
                  </div>
                </div>
              {/each}
            </div>
          {/if}
        </div>
      {/if}

    </div>
  </div>
</div>

<!-- Provision Virtual Device Modal -->
{#if isModalOpen}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/80 backdrop-blur-sm p-4"
    role="dialog"
    aria-modal="true"
  >
    <div class="w-full max-w-md rounded-xl border border-zinc-800 bg-zinc-950 p-6 shadow-2xl text-zinc-100">
      <div class="flex items-center justify-between border-b border-zinc-800 pb-3 mb-4">
        <div class="flex items-center gap-2">
          <Layers class="w-4 h-4 text-cyan-400" />
          <h2 class="text-sm font-semibold text-zinc-100 uppercase tracking-wide">
            Provision Virtual Device (Local Node)
          </h2>
        </div>
        <button
          onclick={() => (isModalOpen = false)}
          class="rounded p-1 text-zinc-400 hover:text-zinc-200"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <div class="space-y-4">
        <!-- Name -->
        <div>
          <label for="virt-name" class="block text-xs font-mono text-zinc-300 mb-1">DEVICE NAME</label>
          <input
            id="virt-name"
            type="text"
            bind:value={newName}
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
          />
        </div>

        <!-- Direction -->
        <div>
          <label for="virt-direction" class="block text-xs font-mono text-zinc-300 mb-1">DIRECTION</label>
          <select
            id="virt-direction"
            bind:value={newDirection}
            class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
          >
            <option value="duplex">Duplex (Input + Output Sink)</option>
            <option value="input">Input Source Only</option>
            <option value="output">Output Sink Only</option>
          </select>
        </div>

        <!-- Channels & Sample Rate -->
        <div class="grid grid-cols-2 gap-3">
          <div>
            <label for="virt-channels" class="block text-xs font-mono text-zinc-300 mb-1">CHANNELS</label>
            <select
              id="virt-channels"
              bind:value={newChannels}
              class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
            >
              <option value={1}>1 (Mono)</option>
              <option value={2}>2 (Stereo)</option>
              <option value={4}>4 (Quad)</option>
              <option value={6}>6 (5.1 Surround)</option>
              <option value={8}>8 (7.1 Surround)</option>
            </select>
          </div>

          <div>
            <label for="virt-rate" class="block text-xs font-mono text-zinc-300 mb-1">SAMPLE RATE</label>
            <select
              id="virt-rate"
              bind:value={newSampleRate}
              class="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm text-zinc-100 focus:border-cyan-500 focus:outline-none"
            >
              <option value={44100}>44.1 kHz</option>
              <option value={48000}>48.0 kHz</option>
              <option value={96000}>96.0 kHz</option>
            </select>
          </div>
        </div>

        {#if formError}
          <div class="rounded-lg bg-red-950/60 border border-red-500/40 p-3 text-xs text-red-300 font-mono">
            {formError}
          </div>
        {/if}

        <div class="mt-6 flex justify-end gap-3 pt-4 border-t border-zinc-800">
          <button
            onclick={() => (isModalOpen = false)}
            class="rounded-lg px-4 py-2 text-xs font-semibold text-zinc-400 hover:text-zinc-200"
          >
            Cancel
          </button>
          <button
            onclick={handleCreateVirtual}
            disabled={isSubmitting || !newName.trim()}
            class="inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-4 py-2 text-xs font-semibold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 disabled:opacity-50"
          >
            {#if isSubmitting}
              <RefreshCw class="w-3.5 h-3.5 animate-spin" />
              <span>Provisioning...</span>
            {:else}
              <Plus class="w-3.5 h-3.5" />
              <span>Create Device</span>
            {/if}
          </button>
        </div>
      </div>
    </div>
  </div>
{/if}
