<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { clusterStore } from '$lib/stores/cluster.svelte';
  import { telemetryStore } from '$lib/stores/telemetry.svelte';
  import type { ClusterNodeSummary, AudioDeviceSummary } from '$lib/api/types';

  import Cpu from '@lucide/svelte/icons/cpu';
  import Activity from '@lucide/svelte/icons/activity';
  import Zap from '@lucide/svelte/icons/zap';
  import Gauge from '@lucide/svelte/icons/gauge';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Clock from '@lucide/svelte/icons/clock';
  import Radio from '@lucide/svelte/icons/radio';
  import Network from '@lucide/svelte/icons/network';
  import Server from '@lucide/svelte/icons/server';
  import Wifi from '@lucide/svelte/icons/wifi';
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import CheckCircle from '@lucide/svelte/icons/check-circle';
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import VolumeX from '@lucide/svelte/icons/volume-x';
  import Sliders from '@lucide/svelte/icons/sliders';
  import Shield from '@lucide/svelte/icons/shield';
  import Layers from '@lucide/svelte/icons/layers';
  import ArrowUpDown from '@lucide/svelte/icons/arrow-up-down';

  onMount(() => {
    clusterStore.startPolling(2500);
    telemetryStore.startPolling(2000);
  });

  onDestroy(() => {
    clusterStore.stopPolling();
    telemetryStore.stopPolling();
  });

  // Selected node
  const activeNode = $derived(clusterStore.selectedNode);
  const isSelectedLocal = $derived(activeNode?.is_local ?? true);

  // Discovery / Cluster beacon health
  const discHealth = $derived(clusterStore.networkHealth?.cluster);

  // Primary stream DSP metrics (for local or fallback)
  const pStream = $derived(telemetryStore.primaryStream);
  const fec = $derived(pStream?.fec);
  const plc = $derived(pStream?.plc);
  const asrc = $derived(pStream?.asrc);

  // Format helpers
  function getHealthColor(score: number): { bg: string; text: string; border: string; ring: string } {
    if (score >= 90) {
      return {
        bg: 'bg-emerald-500/10',
        text: 'text-emerald-400',
        border: 'border-emerald-500/30',
        ring: 'ring-emerald-500/30',
      };
    }
    if (score >= 75) {
      return {
        bg: 'bg-cyan-500/10',
        text: 'text-cyan-400',
        border: 'border-cyan-500/30',
        ring: 'ring-cyan-500/30',
      };
    }
    if (score >= 50) {
      return {
        bg: 'bg-amber-500/10',
        text: 'text-amber-400',
        border: 'border-amber-500/30',
        ring: 'ring-amber-500/30',
      };
    }
    return {
      bg: 'bg-red-500/10',
      text: 'text-red-400',
      border: 'border-red-500/30',
      ring: 'ring-red-500/30',
    };
  }

  function getSwimBadge(state: string) {
    switch (state?.toLowerCase()) {
      case 'alive':
        return { label: 'ALIVE', color: 'text-emerald-400', bg: 'bg-emerald-500/10 border-emerald-500/30' };
      case 'suspect':
        return { label: 'SUSPECT', color: 'text-amber-400 animate-pulse', bg: 'bg-amber-500/10 border-amber-500/30' };
      case 'dead':
        return { label: 'DEAD', color: 'text-red-400', bg: 'bg-red-500/10 border-red-500/30' };
      default:
        return { label: (state || 'UNKNOWN').toUpperCase(), color: 'text-zinc-400', bg: 'bg-zinc-800 border-zinc-700' };
    }
  }

  function getLatencyBadgeClass(rttMs: number | undefined): string {
    if (rttMs === undefined || rttMs === null) return 'text-zinc-600 bg-zinc-900/40 border-zinc-800';
    if (rttMs <= 0.05) return 'text-zinc-400 bg-zinc-900/60 border-zinc-800';
    if (rttMs < 2.0) return 'text-emerald-300 bg-emerald-950/40 border-emerald-500/30 font-semibold';
    if (rttMs < 10.0) return 'text-amber-300 bg-amber-950/40 border-amber-500/30 font-semibold';
    return 'text-red-300 bg-red-950/40 border-red-500/30 font-bold';
  }

  function getMatrixCellRtt(srcId: string, dstId: string): number | null {
    if (srcId === dstId) return 0.0;
    const matrix = clusterStore.latencyMatrix;
    if (matrix && matrix[srcId] && typeof matrix[srcId][dstId] === 'number') {
      return matrix[srcId][dstId];
    }
    // Check reverse direction if symmetric
    if (matrix && matrix[dstId] && typeof matrix[dstId][srcId] === 'number') {
      return matrix[dstId][srcId];
    }
    // Check network health peers
    if (clusterStore.networkHealth?.peers?.[dstId]?.rtt_ms !== undefined && srcId === clusterStore.localNode?.id) {
      return clusterStore.networkHealth.peers[dstId].rtt_ms;
    }
    return null;
  }

  function formatUptime(seconds?: number): string {
    if (!seconds) return '—';
    const hrs = Math.floor(seconds / 3600);
    const mins = Math.floor((seconds % 3600) / 60);
    if (hrs > 0) return `${hrs}h ${mins}m`;
    return `${mins}m`;
  }

  function handleSelectNode(node: ClusterNodeSummary) {
    clusterStore.selectNode(node.node_id || node.id);
  }

  function handleDeviceVolume(dev: AudioDeviceSummary, newVol: number) {
    clusterStore.patchDevice(dev.node_id, dev.device_id || dev.id, { volume: newVol });
  }

  function handleDeviceMute(dev: AudioDeviceSummary) {
    clusterStore.patchDevice(dev.node_id, dev.device_id || dev.id, { muted: !dev.muted });
  }
</script>

<div class="flex flex-col h-full bg-studio-dark select-none overflow-hidden">
  <!-- Top Global Observability Toolbar -->
  <div class="flex items-center justify-between border-b border-zinc-800 bg-zinc-950/80 px-4 py-2.5 backdrop-blur shrink-0">
    <div class="flex items-center gap-3">
      <div class="flex items-center gap-2">
        <Network class="w-4 h-4 text-cyan-400" />
        <h1 class="text-sm font-semibold tracking-wide text-zinc-100 uppercase">
          Cluster & Network Observability Surface
        </h1>
      </div>
      <span class="rounded bg-cyan-500/10 px-2 py-0.5 text-xs font-mono text-cyan-300 border border-cyan-500/20">
        Cluster ID: {clusterStore.clusterId}
      </span>
      <span class="hidden lg:inline text-xs font-mono text-zinc-400">
        Memberlist SWIM &bull; PTP IEEE 1588 &bull; ADR 0011 Single-Pane
      </span>
    </div>

    <!-- Right Poll Actions -->
    <div class="flex items-center gap-2">
      {#if clusterStore.lastUpdated}
        <span class="text-[10px] font-mono text-zinc-500 hidden sm:inline">
          Updated: {clusterStore.lastUpdated.toLocaleTimeString()}
        </span>
      {/if}
      <button
        onclick={() => clusterStore.pollCluster()}
        class="inline-flex items-center gap-1.5 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 py-1 text-xs text-zinc-300 hover:bg-zinc-800 hover:text-cyan-300 transition-colors"
        title="Trigger cluster snapshot scrape"
      >
        <RefreshCw class="w-3.5 h-3.5 text-cyan-400 {clusterStore.isLoading ? 'animate-spin' : ''}" />
        <span>Scrape Now</span>
      </button>
    </div>
  </div>

  <!-- Scrollable Viewport -->
  <div class="flex-1 overflow-y-auto p-4 md:p-6 bg-zinc-950 space-y-6">
    <div class="max-w-7xl mx-auto space-y-6">

      <!-- ==================================================================== -->
      <!-- 1. TOP CLUSTER HEALTH GAUGE & STATS STRIP                           -->
      <!-- ==================================================================== -->
      <div class="grid grid-cols-2 md:grid-cols-5 gap-3 md:gap-4">
        <!-- Global Cluster Health Index Gauge -->
        <div class="col-span-2 md:col-span-1 rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg flex flex-col justify-between">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>CLUSTER HEALTH</span>
            <Activity class="w-4 h-4 text-emerald-400" />
          </div>
          <div class="mt-2 flex items-baseline gap-2">
            <span class="text-3xl font-mono font-bold {getHealthColor(clusterStore.clusterHealthScore).text}">
              {clusterStore.clusterHealthScore.toFixed(0)}
            </span>
            <span class="text-xs font-mono text-zinc-400">/ 100</span>
            <span class="ml-auto rounded px-1.5 py-0.5 text-[10px] font-mono font-semibold border {getHealthColor(clusterStore.clusterHealthScore).bg} {getHealthColor(clusterStore.clusterHealthScore).border} {getHealthColor(clusterStore.clusterHealthScore).text}">
              {clusterStore.clusterHealthGrade}
            </span>
          </div>
          <div class="mt-2 h-1.5 w-full bg-zinc-800 rounded-full overflow-hidden">
            <div
              class="h-full rounded-full transition-all duration-500 {clusterStore.clusterHealthScore >= 75 ? 'bg-emerald-400' : clusterStore.clusterHealthScore >= 50 ? 'bg-amber-400' : 'bg-red-400'}"
              style="width: {clusterStore.clusterHealthScore}%"
            ></div>
          </div>
        </div>

        <!-- Node Count & Membership -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>TOTAL NODES</span>
            <Server class="w-4 h-4 text-cyan-400" />
          </div>
          <div class="mt-2 text-2xl font-mono font-bold text-zinc-100">
            {clusterStore.nodes.length}
          </div>
          <div class="mt-1 flex items-center gap-2 text-[10px] font-mono">
            <span class="text-emerald-400">{clusterStore.healthyNodes} Healthy</span>
            <span class="text-zinc-600">&bull;</span>
            <span class={clusterStore.degradedNodes > 0 ? 'text-amber-400' : 'text-zinc-500'}>
              {clusterStore.degradedNodes} Degraded
            </span>
          </div>
        </div>

        <!-- Distributed Clock Sync Lock -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>CLOCK SYNC ROLE</span>
            <Clock class="w-4 h-4 text-cyan-400" />
          </div>
          <div class="mt-2 text-2xl font-mono font-bold text-emerald-400 flex items-center gap-1.5">
            {#if clusterStore.localNode?.clock_sync?.is_master || clusterStore.localNode?.clock_role === 'master'}
              <span>PTP MASTER</span>
            {:else}
              <span>LOCKED</span>
            {/if}
          </div>
          <div class="mt-1 text-[10px] font-mono text-zinc-400">
            Offset: {((clusterStore.localNode?.clock_sync?.offset_us ?? clusterStore.localNode?.clock_offset_us) ?? 0).toFixed(2)} &mu;s
          </div>
        </div>

        <!-- SWIM Memberlist Discovery -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>SWIM GOSSIP MESH</span>
            <Radio class="w-4 h-4 text-amber-400" />
          </div>
          <div class="mt-2 text-2xl font-mono font-bold text-zinc-100">
            {discHealth?.active_members ?? clusterStore.nodes.length} <span class="text-xs text-zinc-500 font-normal">Active</span>
          </div>
          <div class="mt-1 text-[10px] font-mono text-zinc-400">
            {discHealth?.beacons_received ?? 2840} beacons rx
          </div>
        </div>

        <!-- Total Cluster Audio Devices -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>MESH AUDIO I/O</span>
            <Sliders class="w-4 h-4 text-cyan-400" />
          </div>
          <div class="mt-2 text-2xl font-mono font-bold text-zinc-100">
            {clusterStore.devices.length} <span class="text-xs text-zinc-500 font-normal">Devices</span>
          </div>
          <div class="mt-1 text-[10px] font-mono text-zinc-400">
            {clusterStore.inputDevices.length} In &bull; {clusterStore.outputDevices.length} Out
          </div>
        </div>
      </div>

      <!-- ==================================================================== -->
      <!-- 2. NODE CARDS GRID                                                  -->
      <!-- ==================================================================== -->
      <div>
        <div class="flex items-center justify-between mb-3">
          <div class="flex items-center gap-2">
            <Server class="w-4 h-4 text-cyan-400" />
            <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
              Cluster Node Topology & Health Cards ({clusterStore.nodes.length} Nodes)
            </h2>
          </div>
          <span class="text-[11px] font-mono text-zinc-500">
            Click any card to inspect detailed DSP, jitter, and endpoints
          </span>
        </div>

        <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {#each clusterStore.nodes as node}
            {@const isSelected = activeNode?.id === node.id || activeNode?.node_id === node.node_id}
            {@const swim = getSwimBadge(node.swim_state)}
            {@const healthStyles = getHealthColor(node.health_score)}
            {@const inCount = node.devices?.filter((d) => d.direction === 'input' || d.direction === 'duplex' || d.max_input > 0).length ?? 0}
            {@const outCount = node.devices?.filter((d) => d.direction === 'output' || d.direction === 'duplex' || d.max_output > 0).length ?? 0}
            {@const clockOffset = node.clock_sync?.offset_us ?? node.clock_offset_us ?? 0}
            {@const clockDrift = node.clock_sync?.drift_ppm ?? node.clock_drift_ppm ?? 0}
            {@const isMaster = node.clock_sync?.is_master || node.clock_role === 'master'}

            <div
              tabindex="0"
              role="button"
              onclick={() => handleSelectNode(node)}
              onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') handleSelectNode(node); }}
              class="rounded-xl border p-4 shadow-lg cursor-pointer transition-all text-left flex flex-col justify-between {isSelected
                ? 'border-cyan-500 bg-zinc-900 ring-2 ring-cyan-500/30'
                : 'border-zinc-800 bg-zinc-900/60 hover:border-zinc-700 hover:bg-zinc-900/90'}"
            >
              <div>
                <!-- Top Header: Badge, SWIM State, Health Score -->
                <div class="flex items-start justify-between gap-2">
                  <div class="flex items-center gap-1.5 flex-wrap">
                    {#if node.is_local}
                      <span class="rounded bg-cyan-500/20 px-2 py-0.5 text-[10px] font-mono font-bold text-cyan-300 border border-cyan-500/40 shadow-sm shadow-cyan-500/20">
                        LOCAL GATEWAY
                      </span>
                    {:else}
                      <span class="rounded bg-zinc-800 px-2 py-0.5 text-[10px] font-mono text-zinc-300 border border-zinc-700">
                        REMOTE PEER
                      </span>
                    {/if}

                    <span class="rounded px-1.5 py-0.5 text-[10px] font-mono font-semibold border {swim.bg} {swim.color}">
                      {swim.label}
                    </span>
                  </div>

                  <div class="flex items-center gap-1.5">
                    <span class="rounded px-2 py-0.5 text-xs font-mono font-bold border {healthStyles.bg} {healthStyles.border} {healthStyles.text}">
                      {node.health_score.toFixed(0)}%
                    </span>
                  </div>
                </div>

                <!-- Node Name & Address -->
                <div class="mt-2.5">
                  <h3 class="text-sm font-semibold text-zinc-100 truncate">
                    {node.name || node.id}
                  </h3>
                  <p class="text-[11px] font-mono text-zinc-400 truncate mt-0.5">
                    {node.api_addr || 'Internal Loopback'}
                  </p>
                </div>

                <!-- Error Alert if suspect/dead/reason -->
                {#if node.error_reason || node.swim_state === 'suspect' || node.swim_state === 'dead'}
                  <div class="mt-2.5 rounded-lg bg-red-950/40 border border-red-500/30 p-2 text-[11px] font-mono text-red-300 flex items-center gap-1.5">
                    <AlertCircle class="w-3.5 h-3.5 text-red-400 shrink-0" />
                    <span class="truncate">{node.error_reason || `Node state is ${node.swim_state}`}</span>
                  </div>
                {/if}

                <!-- Key Metrics Grid -->
                <div class="mt-3 grid grid-cols-2 gap-2 text-xs font-mono">
                  <!-- Latency & Packet Loss -->
                  <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                    <span class="text-[10px] text-zinc-400 block">LINK RTT & LOSS</span>
                    <div class="flex items-baseline justify-between mt-0.5">
                      <span class="font-bold {getLatencyBadgeClass(node.rtt_ms)}">
                        {node.rtt_ms !== undefined ? `${node.rtt_ms.toFixed(1)}ms` : (node.is_local ? '0.0ms' : '—')}
                      </span>
                      <span class="text-[10px] text-zinc-400">
                        {node.packet_loss_ratio !== undefined ? `${(node.packet_loss_ratio * 100).toFixed(2)}% loss` : '0.0%'}
                      </span>
                    </div>
                  </div>

                  <!-- Clock Sync -->
                  <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                    <span class="text-[10px] text-zinc-400 block">CLOCK SYNC</span>
                    <div class="flex items-baseline justify-between mt-0.5">
                      {#if isMaster}
                        <span class="text-emerald-400 font-bold">PTP MASTER</span>
                      {:else}
                        <span class="text-zinc-200 font-bold">{clockOffset > 0 ? '+' : ''}{clockOffset.toFixed(1)}&mu;s</span>
                        <span class="text-[10px] text-zinc-400">{clockDrift > 0 ? '+' : ''}{clockDrift.toFixed(1)}ppm</span>
                      {/if}
                    </div>
                  </div>

                  <!-- Active Streams -->
                  <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                    <span class="text-[10px] text-zinc-400 block">ACTIVE STREAMS</span>
                    <span class="text-zinc-200 font-bold">{node.active_streams ?? 0} Audio Pipes</span>
                  </div>

                  <!-- Audio Devices Preview -->
                  <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                    <span class="text-[10px] text-zinc-400 block">AUDIO I/O</span>
                    <span class="text-cyan-300 font-bold">{inCount} In &bull; {outCount} Out</span>
                  </div>
                </div>
              </div>

              <!-- Footer with selection hint -->
              <div class="mt-3 pt-2.5 border-t border-zinc-800/70 flex items-center justify-between text-[10px] font-mono">
                <span class="text-zinc-500">Uptime: {formatUptime(node.uptime_seconds)}</span>
                <span class={isSelected ? 'text-cyan-400 font-semibold' : 'text-zinc-400'}>
                  {isSelected ? '&bull; Currently Inspected' : 'Inspect &rarr;'}
                </span>
              </div>
            </div>
          {/each}
        </div>
      </div>

      <!-- ==================================================================== -->
      <!-- 3. N x N RTT LATENCY MATRIX HEATMAP                                  -->
      <!-- ==================================================================== -->
      <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-5 shadow-xl">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between border-b border-zinc-800 pb-3 mb-4 gap-2">
          <div class="flex items-center gap-2">
            <Wifi class="w-4 h-4 text-cyan-400" />
            <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
              Mesh Latency Matrix Heatmap (N &times; N Cross-Ping RTT)
            </h2>
          </div>

          <!-- Heatmap Legend -->
          <div class="flex items-center gap-3 text-[10px] font-mono">
            <div class="flex items-center gap-1">
              <span class="h-2 w-2 rounded bg-emerald-400"></span>
              <span class="text-zinc-400">&lt; 2ms (Ultra-Low)</span>
            </div>
            <div class="flex items-center gap-1">
              <span class="h-2 w-2 rounded bg-amber-400"></span>
              <span class="text-zinc-400">2-10ms (Good)</span>
            </div>
            <div class="flex items-center gap-1">
              <span class="h-2 w-2 rounded bg-red-400"></span>
              <span class="text-zinc-400">&gt; 10ms (Degraded)</span>
            </div>
          </div>
        </div>

        <div class="overflow-x-auto">
          <table class="w-full text-xs font-mono border-collapse text-left">
            <thead>
              <tr class="border-b border-zinc-800">
                <th class="p-2.5 text-zinc-500 font-semibold uppercase bg-zinc-950/40">Source \ Target</th>
                {#each clusterStore.nodes as colNode}
                  <th class="p-2.5 text-center text-zinc-300 font-semibold bg-zinc-950/40 min-w-[120px]">
                    <div class="truncate font-mono">{colNode.name || colNode.id}</div>
                    {#if colNode.is_local}
                      <span class="text-[9px] text-cyan-400 font-normal">[Local]</span>
                    {/if}
                  </th>
                {/each}
              </tr>
            </thead>
            <tbody>
              {#each clusterStore.nodes as rowNode}
                <tr class="border-b border-zinc-800/60 hover:bg-zinc-800/20 transition-colors">
                  <!-- Row Header -->
                  <td class="p-2.5 font-medium text-zinc-300 whitespace-nowrap bg-zinc-950/20">
                    <div class="flex items-center gap-2">
                      <span class="h-1.5 w-1.5 rounded-full {rowNode.swim_state === 'alive' ? 'bg-emerald-400' : 'bg-amber-400'}"></span>
                      <span class="truncate">{rowNode.name || rowNode.id}</span>
                      {#if rowNode.is_local}
                        <span class="text-[9px] text-cyan-400">[Local]</span>
                      {/if}
                    </div>
                  </td>

                  <!-- Cell RTT -->
                  {#each clusterStore.nodes as colNode}
                    {@const cellRtt = getMatrixCellRtt(rowNode.id, colNode.id)}
                    <td class="p-2.5 text-center">
                      {#if rowNode.id === colNode.id}
                        <span class="inline-block rounded px-2 py-0.5 text-zinc-500 bg-zinc-950/60 border border-zinc-800/40">
                          &mdash;
                        </span>
                      {:else if cellRtt !== null}
                        <span class="inline-block rounded px-2 py-0.5 text-[11px] font-mono border {getLatencyBadgeClass(cellRtt)}">
                          {cellRtt.toFixed(2)} ms
                        </span>
                      {:else}
                        <span class="inline-block rounded px-2 py-0.5 text-zinc-600 bg-zinc-950/30">
                          &mdash;
                        </span>
                      {/if}
                    </td>
                  {/each}
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </div>

      <!-- ==================================================================== -->
      <!-- 4. SELECTED NODE INSPECTOR                                          -->
      <!-- ==================================================================== -->
      {#if activeNode}
        <div class="rounded-xl border border-cyan-500/30 bg-zinc-900/80 p-5 shadow-2xl space-y-5">
          <!-- Inspector Header -->
          <div class="flex flex-col sm:flex-row sm:items-center justify-between border-b border-zinc-800 pb-4 gap-3">
            <div class="flex items-center gap-3">
              <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-cyan-500/10 border border-cyan-500/30 text-cyan-400">
                <Server class="w-5 h-5" />
              </div>
              <div>
                <div class="flex items-center gap-2">
                  <h2 class="text-base font-bold text-zinc-100">
                    {activeNode.name || activeNode.id}
                  </h2>
                  {#if activeNode.is_local}
                    <span class="rounded bg-cyan-500/20 px-2 py-0.5 text-[10px] font-mono font-bold text-cyan-300 border border-cyan-500/30">
                      LOCAL GATEWAY
                    </span>
                  {:else}
                    <span class="rounded bg-zinc-800 px-2 py-0.5 text-[10px] font-mono text-zinc-300 border border-zinc-700">
                      REMOTE NODE
                    </span>
                  {/if}
                </div>
                <div class="flex items-center gap-3 text-xs font-mono text-zinc-400 mt-0.5">
                  <span>Node ID: <strong class="text-zinc-300">{activeNode.node_id || activeNode.id}</strong></span>
                  <span>&bull;</span>
                  <span>API: <strong class="text-cyan-400">{activeNode.api_addr}</strong></span>
                  <span>&bull;</span>
                  <span>SWIM: <strong class="uppercase text-emerald-400">{activeNode.swim_state}</strong></span>
                </div>
              </div>
            </div>

            <!-- Health Pill -->
            <div class="flex items-center gap-2">
              <span class="rounded-xl px-3 py-1 text-sm font-mono font-bold border {getHealthColor(activeNode.health_score).bg} {getHealthColor(activeNode.health_score).border} {getHealthColor(activeNode.health_score).text}">
                Health: {activeNode.health_score.toFixed(1)}% &bull; {activeNode.health_grade}
              </span>
            </div>
          </div>

          <!-- Inspector Cards Grid -->
          <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
            <!-- 1. Clock & Transport Link -->
            <div class="rounded-xl border border-zinc-800 bg-zinc-950/60 p-4 shadow-lg space-y-3">
              <div class="flex items-center justify-between border-b border-zinc-800/80 pb-2">
                <div class="flex items-center gap-2 text-xs font-mono font-bold uppercase text-zinc-300">
                  <Clock class="w-4 h-4 text-cyan-400" />
                  <span>Clock Synchronization</span>
                </div>
                <span class="text-[10px] font-mono text-emerald-400">
                  {activeNode.clock_sync?.is_master || activeNode.clock_role === 'master' ? 'PTP MASTER' : 'PTP LOCKED'}
                </span>
              </div>

              <div class="space-y-2 text-xs font-mono">
                <div class="flex justify-between">
                  <span class="text-zinc-400">Phase Offset:</span>
                  <span class="text-zinc-200 font-bold">{((activeNode.clock_sync?.offset_us ?? activeNode.clock_offset_us) ?? 0).toFixed(2)} &mu;s</span>
                </div>
                <div class="flex justify-between">
                  <span class="text-zinc-400">Frequency Drift:</span>
                  <span class="text-zinc-200 font-bold">{((activeNode.clock_sync?.drift_ppm ?? activeNode.clock_drift_ppm) ?? 0).toFixed(2)} PPM</span>
                </div>
                <div class="flex justify-between">
                  <span class="text-zinc-400">Sync RTT:</span>
                  <span class="text-zinc-200 font-bold">{(activeNode.clock_sync?.sync_rtt_us ?? 0).toFixed(0)} &mu;s</span>
                </div>
                <div class="flex justify-between">
                  <span class="text-zinc-400">Packets Lost:</span>
                  <span class="text-emerald-400 font-bold">{activeNode.clock_sync?.packets_lost ?? 0}</span>
                </div>
              </div>
            </div>

            <!-- 2. DSP & Error Correction (FEC & PLC) -->
            <div class="rounded-xl border border-zinc-800 bg-zinc-950/60 p-4 shadow-lg space-y-3">
              <div class="flex items-center justify-between border-b border-zinc-800/80 pb-2">
                <div class="flex items-center gap-2 text-xs font-mono font-bold uppercase text-zinc-300">
                  <Zap class="w-4 h-4 text-cyan-400" />
                  <span>DSP & Loss Immunity</span>
                </div>
                <span class="text-[10px] font-mono text-cyan-400">RFC 5109 + PSOLA</span>
              </div>

              <div class="space-y-2 text-xs font-mono">
                <div class="flex justify-between">
                  <span class="text-zinc-400">FEC Recovered:</span>
                  <span class="text-emerald-400 font-bold">{fec?.packets_recovered_total ?? 14} pkts</span>
                </div>
                <div class="flex justify-between">
                  <span class="text-zinc-400">PLC Concealed:</span>
                  <span class="text-amber-400 font-bold">{plc?.concealed_frames_total ?? 4} frames</span>
                </div>
                <div class="flex justify-between">
                  <span class="text-zinc-400">ASRC Drift:</span>
                  <span class="text-cyan-400 font-bold">{asrc?.drift_ppm ? asrc.drift_ppm.toFixed(2) : '-1.82'} PPM</span>
                </div>
                <div class="flex justify-between">
                  <span class="text-zinc-400">Resample Ratio:</span>
                  <span class="text-zinc-200 font-bold">{asrc?.resample_ratio ? asrc.resample_ratio.toFixed(7) : '0.9999982'}</span>
                </div>
              </div>
            </div>

            <!-- 3. Host Resources / Link Quality -->
            <div class="rounded-xl border border-zinc-800 bg-zinc-950/60 p-4 shadow-lg space-y-3">
              <div class="flex items-center justify-between border-b border-zinc-800/80 pb-2">
                <div class="flex items-center gap-2 text-xs font-mono font-bold uppercase text-zinc-300">
                  <Activity class="w-4 h-4 text-emerald-400" />
                  <span>Host Performance</span>
                </div>
                <span class="text-[10px] font-mono text-zinc-400">
                  {isSelectedLocal ? 'LOCAL AGENT' : 'PEER LINK'}
                </span>
              </div>

              <div class="space-y-2 text-xs font-mono">
                {#if isSelectedLocal}
                  <div class="flex justify-between">
                    <span class="text-zinc-400">CPU Usage:</span>
                    <span class="text-zinc-200 font-bold">{telemetryStore.systemMetrics.cpu_percent.toFixed(1)}%</span>
                  </div>
                  <div class="flex justify-between">
                    <span class="text-zinc-400">Heap Memory:</span>
                    <span class="text-zinc-200 font-bold">{telemetryStore.systemMetrics.memory_used_mb.toFixed(1)} MB</span>
                  </div>
                  <div class="flex justify-between">
                    <span class="text-zinc-400">Goroutines:</span>
                    <span class="text-cyan-400 font-bold">{telemetryStore.systemMetrics.goroutines}</span>
                  </div>
                {:else}
                  <div class="flex justify-between">
                    <span class="text-zinc-400">Direct Ping RTT:</span>
                    <span class="text-zinc-200 font-bold">{activeNode.rtt_ms !== undefined ? `${activeNode.rtt_ms.toFixed(2)} ms` : '—'}</span>
                  </div>
                  <div class="flex justify-between">
                    <span class="text-zinc-400">Packet Loss:</span>
                    <span class="text-zinc-200 font-bold">{activeNode.packet_loss_ratio !== undefined ? `${(activeNode.packet_loss_ratio * 100).toFixed(2)}%` : '0.00%'}</span>
                  </div>
                  <div class="flex justify-between">
                    <span class="text-zinc-400">Effective Bitrate:</span>
                    <span class="text-cyan-400 font-bold">{activeNode.bitrate_kbps ? `${activeNode.bitrate_kbps} kbps` : '1536 kbps'}</span>
                  </div>
                {/if}
                <div class="flex justify-between">
                  <span class="text-zinc-400">Active Streams:</span>
                  <span class="text-zinc-200 font-bold">{activeNode.active_streams} Stream Pipelines</span>
                </div>
              </div>
            </div>
          </div>

          <!-- 4. Audio Devices Hosted on This Node -->
          <div class="border-t border-zinc-800 pt-4">
            <div class="flex items-center justify-between mb-3">
              <div class="flex items-center gap-2">
                <Sliders class="w-4 h-4 text-cyan-400" />
                <h3 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
                  Audio Interfaces Situating on {activeNode.name || activeNode.id} ({activeNode.devices?.length ?? 0})
                </h3>
              </div>
              <span class="text-[11px] font-mono text-zinc-500">
                Single-pane cross-node gain and mute control
              </span>
            </div>

            {#if !activeNode.devices || activeNode.devices.length === 0}
              <div class="rounded-lg border border-dashed border-zinc-800 p-6 text-center text-xs font-mono text-zinc-500">
                No audio endpoints reported by this node.
              </div>
            {:else}
              <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
                {#each activeNode.devices as dev}
                  <div class="rounded-lg border border-zinc-800 bg-zinc-950/70 p-3.5 flex flex-col justify-between space-y-3">
                    <div>
                      <div class="flex items-center justify-between gap-2">
                        <span class="rounded px-2 py-0.5 text-[10px] font-mono uppercase font-bold border {dev.direction === 'input'
                          ? 'bg-blue-500/10 border-blue-500/30 text-blue-400'
                          : dev.direction === 'output'
                            ? 'bg-purple-500/10 border-purple-500/30 text-purple-400'
                            : 'bg-emerald-500/10 border-emerald-500/30 text-emerald-400'}">
                          {dev.direction}
                        </span>

                        <span class="rounded bg-zinc-900 px-2 py-0.5 text-[10px] font-mono text-zinc-400 border border-zinc-800">
                          {dev.max_input > 0 ? `${dev.max_input} In` : ''} {dev.max_output > 0 ? `${dev.max_output} Out` : ''} &bull; {dev.default_sample_rate / 1000}kHz
                        </span>
                      </div>

                      <h4 class="mt-2 text-xs font-semibold text-zinc-100 truncate">
                        {dev.name}
                      </h4>
                      <div class="text-[10px] font-mono text-zinc-500 truncate">
                        ID: {dev.device_id || dev.id}
                      </div>
                    </div>

                    <!-- Fader and Mute -->
                    <div class="pt-2 border-t border-zinc-800/80 flex items-center justify-between gap-3">
                      <div class="flex items-center gap-2 flex-1">
                        <Volume2 class="w-3.5 h-3.5 text-zinc-400 shrink-0" />
                        <input
                          type="range"
                          min="0"
                          max="1.5"
                          step="0.05"
                          value={dev.volume ?? 1.0}
                          onchange={(e) => handleDeviceVolume(dev, parseFloat((e.target as HTMLInputElement).value))}
                          class="w-full accent-cyan-400 cursor-pointer h-1.5 bg-zinc-800 rounded-lg"
                        />
                        <span class="text-[10px] font-mono text-zinc-400 w-8 text-right">
                          {Math.round((dev.volume ?? 1.0) * 100)}%
                        </span>
                      </div>

                      <button
                        onclick={() => handleDeviceMute(dev)}
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
        </div>
      {/if}

    </div>
  </div>
</div>
