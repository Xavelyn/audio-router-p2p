<script lang="ts">
  import { onMount } from 'svelte';
  import { matrixStore, type CrosspointCell } from '$lib/stores/matrix.svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import { formatDb } from '$lib/api/types';
  import GainFaderPopover from './GainFaderPopover.svelte';
  import Sliders from '@lucide/svelte/icons/sliders';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Search from '@lucide/svelte/icons/search';
  import Zap from '@lucide/svelte/icons/zap';
  import Volume2 from '@lucide/svelte/icons/volume-2';

  let filterText = $state('');
  let isClearing = $state(false);

  onMount(() => {
    matrixStore.loadRoutes();
    deviceStore.loadDevices();
  });

  // Filtered Input Ports (Columns)
  let inputPorts = $derived.by(() => {
    let list = deviceStore.matrixInputPorts;
    if (filterText.trim()) {
      const q = filterText.toLowerCase();
      list = list.filter(
        (p) =>
          p.deviceName.toLowerCase().includes(q) ||
          p.nodeId.toLowerCase().includes(q) ||
          p.channelLabel.toLowerCase().includes(q)
      );
    }
    return list;
  });

  // Filtered Output Ports (Rows)
  let outputPorts = $derived.by(() => {
    let list = deviceStore.matrixOutputPorts;
    if (filterText.trim()) {
      const q = filterText.toLowerCase();
      list = list.filter(
        (p) =>
          p.deviceName.toLowerCase().includes(q) ||
          p.nodeId.toLowerCase().includes(q) ||
          p.channelLabel.toLowerCase().includes(q)
      );
    }
    return list;
  });

  function getCell(outPortId: string, inPortId: string): CrosspointCell | undefined {
    const cpKey = `${outPortId}->${inPortId}`;
    return matrixStore.crosspointMap[cpKey];
  }

  async function handleCellClick(e: MouseEvent, inPort: any, outPort: any) {
    if (e.shiftKey || e.altKey) {
      // Open fader on shift-click
      openFader(e, inPort, outPort);
      return;
    }
    await matrixStore.toggleCrosspoint(
      { nodeId: inPort.nodeId, deviceId: inPort.deviceId, channel: inPort.channel },
      { nodeId: outPort.nodeId, deviceId: outPort.deviceId, channel: outPort.channel }
    );
  }

  function handleContextMenu(e: MouseEvent, inPort: any, outPort: any) {
    e.preventDefault();
    openFader(e, inPort, outPort);
  }

  function openFader(e: MouseEvent, inPort: any, outPort: any) {
    const cpKey = `${outPort.id}->${inPort.id}`;
    let cell = matrixStore.crosspointMap[cpKey];

    if (!cell) {
      // If cell is not yet connected, synthesize an initial connection at unity so fader can edit it
      cell = {
        routeId: `pending-${Date.now()}`,
        inNodeId: inPort.nodeId,
        inDeviceId: inPort.deviceId,
        inChannel: inPort.channel,
        outNodeId: outPort.nodeId,
        outDeviceId: outPort.deviceId,
        outChannel: outPort.channel,
        gainLinear: 1.0,
        gainDb: 0.0,
        muted: false,
        isSlewing: false,
      };
      matrixStore.toggleCrosspoint(
        { nodeId: inPort.nodeId, deviceId: inPort.deviceId, channel: inPort.channel },
        { nodeId: outPort.nodeId, deviceId: outPort.deviceId, channel: outPort.channel }
      );
    }

    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    matrixStore.openFaderPopover(cell, { x: rect.right, y: rect.top });
  }

  // Quick 1:1 Identity Routing preset
  async function routeIdentity() {
    const minLen = Math.min(inputPorts.length, outputPorts.length);
    for (let i = 0; i < minLen; i++) {
      const inPort = inputPorts[i];
      const outPort = outputPorts[i];
      const cell = getCell(outPort.id, inPort.id);
      if (!cell) {
        await matrixStore.toggleCrosspoint(
          { nodeId: inPort.nodeId, deviceId: inPort.deviceId, channel: inPort.channel },
          { nodeId: outPort.nodeId, deviceId: outPort.deviceId, channel: outPort.channel }
        );
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
        {Object.keys(matrixStore.crosspointMap).length} Active Points
      </span>
    </div>

    <!-- Search / Filter -->
    <div class="flex items-center gap-2">
      <div class="relative">
        <Search class="absolute left-2.5 top-2 w-3.5 h-3.5 text-zinc-500" />
        <input
          type="text"
          bind:value={filterText}
          placeholder="Filter channels or devices..."
          class="h-7.5 w-56 rounded-md border border-zinc-800 bg-zinc-900/90 pl-8 pr-3 text-xs font-mono text-zinc-200 placeholder-zinc-500 focus:border-cyan-500 focus:outline-none"
        />
      </div>

      <!-- Quick Action Buttons -->
      <button
        onclick={routeIdentity}
        class="inline-flex items-center gap-1.5 h-7.5 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 text-xs font-medium text-zinc-300 hover:bg-zinc-800 hover:text-cyan-300 transition-colors"
        title="Connect input N to output N at unity"
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

  <!-- Instruction bar -->
  <div class="flex items-center justify-between border-b border-zinc-800/60 bg-zinc-900/40 px-4 py-1 text-[11px] font-mono text-zinc-400">
    <div class="flex items-center gap-4">
      <span><strong class="text-zinc-200">Left Click:</strong> Connect / Disconnect (Unity 0 dB)</span>
      <span><strong class="text-zinc-200">Right Click / Shift+Click:</strong> Open Gain Fader</span>
    </div>
    <div class="flex items-center gap-2">
      <span class="inline-block h-2 w-2 rounded-full bg-cyan-400 shadow-[0_0_6px_rgba(6,182,212,0.8)]"></span>
      <span>Active Connection</span>
      <span class="inline-block h-2 w-2 rounded-full bg-zinc-700 ml-2"></span>
      <span>Disconnected</span>
    </div>
  </div>

  <!-- Matrix Grid Canvas Container -->
  <div class="flex-1 overflow-auto relative p-4 bg-zinc-950">
    {#if inputPorts.length === 0 || outputPorts.length === 0}
      <div class="flex h-64 flex-col items-center justify-center text-zinc-500 text-xs">
        <p>No audio input or output ports available.</p>
        <p class="mt-1 text-zinc-600">Ensure audio interfaces are connected or provision a Virtual Device.</p>
      </div>
    {:else}
      <div class="inline-block min-w-full">
        <table class="border-collapse table-fixed select-none">
          <thead>
            <tr>
              <!-- Top-Left Corner Header -->
              <th
                class="sticky top-0 left-0 z-30 bg-zinc-900 border border-zinc-800 p-2 text-left min-w-[200px] w-[200px]"
              >
                <div class="text-[10px] font-mono font-bold text-zinc-400 uppercase">
                  Inputs &rarr; / Outputs &darr;
                </div>
              </th>

              <!-- Sticky Column Headers (Inputs) -->
              {#each inputPorts as inPort}
                <th
                  class="sticky top-0 z-20 bg-zinc-900 border border-zinc-800 p-1.5 w-14 min-w-[56px] max-w-[56px] text-center"
                  title="{inPort.deviceName} - {inPort.channelLabel} ({inPort.nodeId})"
                >
                  <div class="flex flex-col items-center justify-end h-24">
                    <span class="text-[9px] font-mono text-zinc-500 truncate w-full text-center">
                      {inPort.isVirtual ? 'VIRT' : 'HW'}
                    </span>
                    <span class="text-[10px] font-mono text-zinc-300 truncate w-full text-center font-medium" title={inPort.deviceName}>
                      {inPort.deviceName.slice(0, 7)}
                    </span>
                    <span class="mt-1 rounded bg-zinc-800 px-1 py-0.5 text-[10px] font-mono font-bold text-cyan-400">
                      {inPort.channelLabel}
                    </span>
                  </div>
                </th>
              {/each}
            </tr>
          </thead>

          <tbody>
            <!-- Rows (Outputs) -->
            {#each outputPorts as outPort}
              <tr>
                <!-- Sticky Row Header (Output destination) -->
                <td
                  class="sticky left-0 z-10 bg-zinc-900 border border-zinc-800 px-2 py-1.5 min-w-[200px] w-[200px]"
                  title="{outPort.deviceName} - {outPort.channelLabel}"
                >
                  <div class="flex items-center justify-between">
                    <div class="flex flex-col truncate pr-2">
                      <span class="text-xs font-mono text-zinc-200 font-medium truncate">
                        {outPort.deviceName}
                      </span>
                      <span class="text-[10px] font-mono text-zinc-500">
                        {outPort.nodeId} &bull; {outPort.isVirtual ? 'Virtual' : 'Hardware'}
                      </span>
                    </div>
                    <span class="rounded bg-zinc-800 px-1.5 py-0.5 text-xs font-mono font-bold text-cyan-400 shrink-0">
                      {outPort.channelLabel}
                    </span>
                  </div>
                </td>

                <!-- Crosspoint Intersection Cells -->
                {#each inputPorts as inPort}
                  {@const cell = getCell(outPort.id, inPort.id)}
                  {@const isConnected = !!cell}
                  {@const isMuted = cell?.muted ?? false}
                  <td class="border border-zinc-800/80 p-0 text-center w-14 min-w-[56px] max-w-[56px] h-12 bg-zinc-950/60 hover:bg-zinc-800/40 transition-colors">
                    <button
                      type="button"
                      onclick={(e) => handleCellClick(e, inPort, outPort)}
                      oncontextmenu={(e) => handleContextMenu(e, inPort, outPort)}
                      class="w-full h-full flex flex-col items-center justify-center p-1 group focus:outline-none focus:ring-1 focus:ring-cyan-500/50"
                      title={isConnected
                        ? `Connected: ${cell.inChannel} -> ${cell.outChannel} (${formatDb(cell.gainDb)}) - Click to disconnect, Right-click to adjust`
                        : `Disconnected - Click to patch at 0 dB`}
                    >
                      {#if isConnected}
                        <!-- Active Crosspoint Button -->
                        <div
                          class="w-8 h-8 rounded flex flex-col items-center justify-center border transition-all cursor-pointer {isMuted
                            ? 'bg-red-500/20 border-red-500/60 text-red-400 shadow-[0_0_8px_rgba(239,68,68,0.4)]'
                            : cell.isSlewing
                            ? 'bg-amber-500/20 border-amber-400 text-amber-300 shadow-[0_0_12px_rgba(245,158,11,0.6)] animate-pulse'
                            : 'bg-cyan-500/20 border-cyan-400 text-cyan-300 shadow-[0_0_10px_rgba(6,182,212,0.5)] group-hover:bg-cyan-500/30'}"
                        >
                          <span class="text-[9px] font-mono font-bold leading-tight">
                            {isMuted ? 'MUTE' : cell.gainDb === 0 ? '0 dB' : formatDb(cell.gainDb, 0)}
                          </span>
                        </div>
                      {:else}
                        <!-- Inactive Crosspoint Dot -->
                        <div
                          class="w-2 h-2 rounded-full bg-zinc-800 group-hover:bg-cyan-500/50 group-hover:scale-150 transition-all"
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
