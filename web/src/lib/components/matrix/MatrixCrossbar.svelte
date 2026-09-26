<script lang="ts">
  import { onMount } from 'svelte';
  import { matrixStore, type CrosspointCell } from '$lib/stores/matrix.svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import type { AudioDevice } from '$lib/api/types';
  import { formatDb } from '$lib/api/types';
  import GainFaderPopover from './GainFaderPopover.svelte';
  import Sliders from '@lucide/svelte/icons/sliders';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Search from '@lucide/svelte/icons/search';
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import X from '@lucide/svelte/icons/x';

  let filterText = $state('');
  let isClearing = $state(false);

  onMount(() => {
    matrixStore.loadRoutes();
    deviceStore.loadDevices();
  });

  // Filtered Source Devices (Columns: max_input > 0)
  let sourceDevices = $derived.by<AudioDevice[]>(() => {
    let list = deviceStore.matrixSourceDevices;
    if (filterText.trim()) {
      const q = filterText.toLowerCase();
      list = list.filter(
        (d) =>
          d.name.toLowerCase().includes(q) ||
          d.id.toLowerCase().includes(q)
      );
    }
    return list;
  });

  // Filtered Destination Devices (Rows: max_output > 0)
  let destinationDevices = $derived.by<AudioDevice[]>(() => {
    let list = deviceStore.matrixDestinationDevices;
    if (filterText.trim()) {
      const q = filterText.toLowerCase();
      list = list.filter(
        (d) =>
          d.name.toLowerCase().includes(q) ||
          d.id.toLowerCase().includes(q)
      );
    }
    return list;
  });

  function formatChannels(count: number): string {
    if (count === 1) return 'Mono';
    if (count === 2) return 'Stereo';
    return `${count} Ch`;
  }

  function getCell(destDeviceId: string, sourceDeviceId: string): CrosspointCell | undefined {
    return matrixStore.getCrosspoint(destDeviceId, sourceDeviceId);
  }

  async function handleCellClick(e: MouseEvent, src: AudioDevice, dst: AudioDevice) {
    if (e.shiftKey || e.altKey) {
      openFader(e, src, dst);
      return;
    }
    await matrixStore.toggleCrosspoint(src, dst);
  }

  function handleContextMenu(e: MouseEvent, src: AudioDevice, dst: AudioDevice) {
    e.preventDefault();
    openFader(e, src, dst);
  }

  function openFader(e: MouseEvent, src: AudioDevice, dst: AudioDevice) {
    let cell = matrixStore.getCrosspoint(dst.id, src.id);

    if (!cell) {
      // Connect first, then open fader once established
      matrixStore.toggleCrosspoint(src, dst).then(() => {
        const createdCell = matrixStore.getCrosspoint(dst.id, src.id);
        if (createdCell) {
          const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
          matrixStore.openFaderPopover(createdCell, { x: rect.right, y: rect.top }, src, dst);
        }
      });
      return;
    }

    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    matrixStore.openFaderPopover(cell, { x: rect.right, y: rect.top }, src, dst);
  }

  // Quick 1:1 Identity Routing preset across devices
  async function routeIdentity() {
    const minLen = Math.min(sourceDevices.length, destinationDevices.length);
    for (let i = 0; i < minLen; i++) {
      const src = sourceDevices[i];
      const dst = destinationDevices[i];
      const cell = getCell(dst.id, src.id);
      if (!cell) {
        await matrixStore.toggleCrosspoint(src, dst);
      }
    }
  }

  // Disconnect all active crosspoints
  async function disconnectAll() {
    isClearing = true;
    for (const route of [...matrixStore.routes]) {
      await matrixStore.deleteRoute(route.id);
    }
    isClearing = false;
  }
</script>

<div class="flex flex-col h-full bg-studio-dark select-none overflow-hidden">
  <!-- Crossbar Toolbar -->
  <div class="flex flex-wrap items-center justify-between gap-3 border-b border-zinc-800 bg-zinc-950/70 px-4 py-2.5">
    <div class="flex items-center gap-3">
      <div class="flex items-center gap-2">
        <Sliders class="w-4 h-4 text-cyan-400" />
        <h1 class="text-sm font-semibold tracking-wide text-zinc-100 uppercase">
          Routing Matrix Crossbar
        </h1>
      </div>
      <span class="rounded bg-zinc-800 px-2 py-0.5 text-xs font-mono text-zinc-300">
        {Object.keys(matrixStore.crosspointMap).length} Active Routes
      </span>
    </div>

    <!-- Search / Filter & Actions -->
    <div class="flex items-center gap-2">
      <div class="relative">
        <Search class="absolute left-2.5 top-2 w-3.5 h-3.5 text-zinc-500" />
        <input
          type="text"
          bind:value={filterText}
          placeholder="Filter devices..."
          class="h-7.5 w-56 rounded-md border border-zinc-800 bg-zinc-900/90 pl-8 pr-3 text-xs font-mono text-zinc-200 placeholder-zinc-500 focus:border-cyan-500 focus:outline-none"
        />
      </div>

      <!-- Quick Action Buttons -->
      <button
        onclick={routeIdentity}
        class="inline-flex items-center gap-1.5 h-7.5 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 text-xs font-medium text-zinc-300 hover:bg-zinc-800 hover:text-cyan-300 transition-colors"
        title="Connect source N to destination N at unity"
      >
        <span>1:1 Patch</span>
      </button>

      <button
        onclick={disconnectAll}
        disabled={isClearing}
        class="inline-flex items-center gap-1.5 h-7.5 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 text-xs font-medium text-red-400 hover:bg-red-950/40 hover:border-red-800/60 transition-colors disabled:opacity-50"
        title="Clear all active patch connections"
      >
        <span>Clear All</span>
      </button>

      <button
        onclick={() => {
          matrixStore.loadRoutes();
          deviceStore.loadDevices();
        }}
        class="inline-flex items-center justify-center h-7.5 w-7.5 rounded-md border border-zinc-800 bg-zinc-900 text-zinc-400 hover:text-zinc-200 hover:bg-zinc-800 transition-colors"
        title="Reload routes from daemon"
      >
        <RefreshCw class="w-3.5 h-3.5 {matrixStore.isLoading ? 'animate-spin text-cyan-400' : ''}" />
      </button>
    </div>
  </div>

  <!-- Prominent Error Banner -->
  {#if matrixStore.lastError}
    <div
      role="alert"
      class="flex items-center justify-between gap-3 border-b border-red-500/50 bg-gradient-to-r from-red-950/95 via-red-900/90 to-red-950/95 px-4 py-2.5 text-xs text-red-200 shadow-lg shadow-red-950/50 shrink-0 animate-in fade-in slide-in-from-top-2"
    >
      <div class="flex items-center gap-2.5 min-w-0">
        <AlertCircle class="w-4 h-4 text-red-400 shrink-0 animate-pulse" />
        <div class="flex flex-col sm:flex-row sm:items-center sm:gap-2 truncate">
          <span class="font-bold uppercase tracking-wider text-red-300 text-[10px] shrink-0">Routing Error:</span>
          <span class="font-mono text-xs text-red-100 truncate">{matrixStore.lastError}</span>
        </div>
      </div>
      <button
        type="button"
        onclick={() => matrixStore.clearError()}
        class="inline-flex items-center gap-1 rounded border border-red-700/60 bg-red-900/60 px-2.5 py-1 text-[11px] font-medium text-red-100 hover:bg-red-800 hover:border-red-500 transition-colors shrink-0"
        aria-label="Dismiss error banner"
      >
        <X class="w-3.5 h-3.5" />
        <span>Dismiss</span>
      </button>
    </div>
  {/if}

  <!-- Instruction bar -->
  <div class="flex items-center justify-between border-b border-zinc-800/60 bg-zinc-900/40 px-4 py-1 text-[11px] font-mono text-zinc-400">
    <div class="flex items-center gap-4">
      <span><strong class="text-zinc-200">Left Click:</strong> Connect / Disconnect (Unity 0 dB)</span>
      <span><strong class="text-zinc-200">Right Click / Shift+Click:</strong> Open Route Gain Fader</span>
    </div>
    <div class="flex items-center gap-2">
      <span class="inline-block h-2 w-2 rounded-full bg-cyan-400 shadow-[0_0_6px_rgba(6,182,212,0.8)]"></span>
      <span>Connected Route</span>
      <span class="inline-block h-2 w-2 rounded-full bg-zinc-700 ml-2"></span>
      <span>Disconnected</span>
    </div>
  </div>

  <!-- Matrix Grid Canvas Container -->
  <div class="flex-1 overflow-auto relative p-4 bg-zinc-950">
    {#if sourceDevices.length === 0 || destinationDevices.length === 0}
      <div class="flex h-72 flex-col items-center justify-center text-center p-6 text-zinc-400">
        <Sliders class="w-10 h-10 text-zinc-600 mb-3" />
        <h3 class="text-sm font-semibold text-zinc-200">Matrix Endpoints Incomplete</h3>
        {#if sourceDevices.length === 0 && destinationDevices.length === 0}
          <p class="mt-1 text-xs text-zinc-500 max-w-md">
            No audio input sources or output destinations were found. Ensure audio devices are attached or create a Virtual Device.
          </p>
        {:else if sourceDevices.length === 0}
          <p class="mt-1 text-xs text-zinc-500 max-w-md">
            No source devices with audio inputs available (<code class="text-cyan-400">max_input &gt; 0</code>). Devices without input channels cannot serve as matrix source columns.
          </p>
        {:else}
          <p class="mt-1 text-xs text-zinc-500 max-w-md">
            No destination devices with audio outputs available (<code class="text-cyan-400">max_output &gt; 0</code>). Devices without output channels cannot serve as matrix destination rows.
          </p>
        {/if}
      </div>
    {:else}
      <div class="inline-block min-w-full">
        <table class="border-collapse table-fixed select-none">
          <thead>
            <tr>
              <!-- Top-Left Corner Header -->
              <th
                class="sticky top-0 left-0 z-30 bg-zinc-900 border border-zinc-800 p-2 text-left min-w-[220px] w-[220px]"
              >
                <div class="text-[10px] font-mono font-bold text-zinc-400 uppercase tracking-wider">
                  Inputs (Sources) &rarr;<br />
                  Outputs (Dests) &darr;
                </div>
              </th>

              <!-- Sticky Column Headers (Source Devices / Inputs) -->
              {#each sourceDevices as src}
                <th
                  class="sticky top-0 z-20 bg-zinc-900 border border-zinc-800 p-2 w-28 min-w-[112px] max-w-[112px] text-center"
                  title="{src.name} ({src.id}) - {formatChannels(src.max_input)} input"
                >
                  <div class="flex flex-col items-center justify-end h-28 gap-1.5">
                    <span class="rounded px-1.5 py-0.5 text-[9px] font-mono font-bold {src.is_virtual ? 'bg-purple-950/80 text-purple-300 border border-purple-800/60' : 'bg-emerald-950/80 text-emerald-300 border border-emerald-800/60'}">
                      {src.is_virtual ? 'VIRT' : 'HW'}
                    </span>
                    <span class="text-[11px] font-mono text-zinc-200 truncate w-full text-center font-medium leading-tight" title={src.name}>
                      {src.name}
                    </span>
                    <span class="rounded bg-zinc-800/90 px-1.5 py-0.5 text-[10px] font-mono font-bold text-cyan-400">
                      {formatChannels(src.max_input)}
                    </span>
                  </div>
                </th>
              {/each}
            </tr>
          </thead>

          <tbody>
            <!-- Rows (Destination Devices / Outputs) -->
            {#each destinationDevices as dst}
              <tr>
                <!-- Sticky Row Header (Output destination) -->
                <td
                  class="sticky left-0 z-10 bg-zinc-900 border border-zinc-800 px-3 py-2 min-w-[220px] w-[220px]"
                  title="{dst.name} ({dst.id}) - {formatChannels(dst.max_output)} output"
                >
                  <div class="flex items-center justify-between gap-2">
                    <div class="flex flex-col truncate min-w-0 pr-1">
                      <span class="text-xs font-mono text-zinc-200 font-medium truncate" title={dst.name}>
                        {dst.name}
                      </span>
                      <div class="flex items-center gap-1.5 mt-0.5">
                        <span class="rounded px-1 py-0.2 text-[9px] font-mono font-bold {dst.is_virtual ? 'bg-purple-950/80 text-purple-300 border border-purple-800/60' : 'bg-emerald-950/80 text-emerald-300 border border-emerald-800/60'}">
                          {dst.is_virtual ? 'VIRT' : 'HW'}
                        </span>
                        <span class="text-[10px] font-mono text-zinc-500 truncate">
                          {dst.id}
                        </span>
                      </div>
                    </div>
                    <span class="rounded bg-zinc-800/90 px-1.5 py-0.5 text-xs font-mono font-bold text-cyan-400 shrink-0">
                      {formatChannels(dst.max_output)}
                    </span>
                  </div>
                </td>

                <!-- Crosspoint Intersection Cells -->
                {#each sourceDevices as src}
                  {@const cell = getCell(dst.id, src.id)}
                  {@const isConnected = !!cell}
                  {@const isMuted = cell?.muted ?? false}
                  <td class="border border-zinc-800/80 p-0 text-center w-28 min-w-[112px] max-w-[112px] h-14 bg-zinc-950/60 hover:bg-zinc-800/30 transition-colors">
                    <button
                      type="button"
                      onclick={(e) => handleCellClick(e, src, dst)}
                      oncontextmenu={(e) => handleContextMenu(e, src, dst)}
                      class="w-full h-full flex flex-col items-center justify-center p-1.5 group focus:outline-none focus:ring-1 focus:ring-cyan-500/50"
                      title={isConnected
                        ? `Connected: ${src.name} -> ${dst.name} (${formatDb(cell.gainDb)}) - Click to disconnect, Right-click to adjust gain`
                        : `Disconnected: ${src.name} -> ${dst.name} - Click to patch at 0 dB`}
                    >
                      {#if isConnected}
                        <!-- Active Crosspoint Indicator -->
                        <div
                          class="w-16 h-8 rounded-md flex items-center justify-center border transition-all cursor-pointer {isMuted
                            ? 'bg-red-500/20 border-red-500/60 text-red-400 shadow-[0_0_8px_rgba(239,68,68,0.4)]'
                            : cell.isSlewing
                            ? 'bg-amber-500/20 border-amber-400 text-amber-300 shadow-[0_0_12px_rgba(245,158,11,0.6)] animate-pulse'
                            : 'bg-cyan-500/20 border-cyan-400 text-cyan-300 shadow-[0_0_10px_rgba(6,182,212,0.5)] group-hover:bg-cyan-500/30 group-hover:scale-105'}"
                        >
                          <span class="text-[10px] font-mono font-bold leading-tight">
                            {isMuted ? 'MUTE' : cell.gainDb === 0 ? '0 dB' : formatDb(cell.gainDb, 0)}
                          </span>
                        </div>
                      {:else}
                        <!-- Inactive Crosspoint Dot -->
                        <div
                          class="w-2.5 h-2.5 rounded-full bg-zinc-800 group-hover:bg-cyan-500/50 group-hover:scale-150 transition-all"
                        ></div>
                      {/if}
                    </button>
                  </td>
                {/each}
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>

<GainFaderPopover />
