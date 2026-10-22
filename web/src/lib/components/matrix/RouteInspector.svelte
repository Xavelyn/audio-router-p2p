<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { matrixStore } from '$lib/stores/matrix.svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import { apiClient } from '$lib/api/client';
  import type { RouteStatus, CalibrateResult, CalibrateRequest } from '$lib/api/types';
  import X from '@lucide/svelte/icons/x';
  import Activity from '@lucide/svelte/icons/activity';
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import CheckCircle from '@lucide/svelte/icons/check-circle';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Gauge from '@lucide/svelte/icons/gauge';

  let { routeId }: { routeId: string } = $props();

  // ------------------------------------------------------------------
  // Route identity (read-only, from the loaded matrix routes)
  // ------------------------------------------------------------------
  const route = $derived(matrixStore.routes.find((r) => r.id === routeId));
  const channelCount = $derived(route?.in_channels || route?.out_channels || 2);

  function deviceLabel(deviceId?: string, nodeId?: string): string {
    const name = deviceStore.devices.find((d) => d.id === deviceId)?.name;
    const base = name || deviceId || '?';
    return nodeId && nodeId !== 'local' ? `${base}@${nodeId}` : base;
  }
  const srcLabel = $derived(route ? deviceLabel(route.source.device_id, route.source.node_id) : 'source');
  const dstLabel = $derived(route ? deviceLabel(route.destination.device_id, route.destination.node_id) : 'destination');

  // ------------------------------------------------------------------
  // Live route status (polled while the drawer is open)
  // ------------------------------------------------------------------
  let status = $state<RouteStatus | null>(null);
  let statusError = $state<string | null>(null);
  let routeNotFound = $state(false);

  async function loadStatus(id: string) {
    try {
      status = await apiClient.getRouteStatus(id);
      statusError = null;
      routeNotFound = false;
    } catch (err: any) {
      if (err?.status === 404) {
        routeNotFound = true;
        status = null;
        statusError = null;
      } else if (err?.status === 501) {
        statusError = 'Route status is not available on this daemon.';
        status = null;
        routeNotFound = false;
      } else {
        statusError = err?.message || 'Failed to load route status.';
      }
    }
  }

  // ------------------------------------------------------------------
  // Calibration
  // ------------------------------------------------------------------
  let calibrating = $state(false);
  let calibrateResult = $state<CalibrateResult | null>(null);
  let calibrateError = $state<string | null>(null);
  let showAdvanced = $state(false);
  let probeFreq = $state(0);
  let probeAmp = $state(0);
  let probeDur = $state(0);

  async function doCalibrate(apply: boolean) {
    if (calibrating) return;
    calibrating = true;
    calibrateError = null;
    calibrateResult = null;
    const req: CalibrateRequest = { apply };
    if (probeFreq) req.freq_hz = probeFreq;
    if (probeAmp) req.amplitude = probeAmp;
    if (probeDur) req.duration_ms = probeDur;
    try {
      const res = await apiClient.calibrateRoute(routeId, req);
      calibrateResult = res;
      if (res.applied && res.offsets_ms) {
        alignOffsets = padOffsets(res.offsets_ms);
      }
    } catch (err: any) {
      if (err?.status === 404) {
        calibrateError = 'Route is not running.';
      } else if (err?.status === 501) {
        calibrateError =
          err?.code === 'ERR_NOT_CALIBRATABLE'
            ? 'This route can\u2019t be calibrated \u2014 it needs input + output on this node. You can still set alignment manually below.'
            : 'Calibration is not available on this daemon.';
      } else {
        calibrateError = err?.message || 'Calibration failed.';
      }
    } finally {
      calibrating = false;
    }
  }

  // ------------------------------------------------------------------
  // Per-channel alignment
  // ------------------------------------------------------------------
  let alignOffsets = $state<number[]>([]);
  let alignError = $state<string | null>(null);
  let alignDisabled = $state(false);
  let alignDisabledReason = $state<string | null>(null);

  function padOffsets(loaded: number[]): number[] {
    const n = channelCount;
    const arr: number[] = new Array(n).fill(0);
    for (let i = 0; i < n; i++) arr[i] = loaded[i] ?? 0;
    return arr;
  }

  async function loadAlignment(id: string) {
    try {
      const info = await apiClient.getRouteAlignment(id);
      alignOffsets = padOffsets(info.offsets_ms ?? []);
    } catch (err: any) {
      if (err?.status === 404) {
        // Route not in the table either; show a zero baseline.
        alignOffsets = new Array(channelCount).fill(0);
      } else if (err?.status === 501) {
        alignDisabled = true;
        alignDisabledReason = 'Alignment is not available on this daemon.';
      } else {
        alignError = err?.message || 'Failed to load alignment offsets.';
      }
    }
  }

  async function applyAlignment() {
    if (!alignOffsets.length) return;
    alignError = null;
    const offsets = alignOffsets.map((v) => (v === undefined || Number.isNaN(v) ? 0 : v));
    try {
      const info = await apiClient.setRouteAlignment(routeId, { offsets_ms: offsets });
      alignOffsets = padOffsets(info.offsets_ms ?? offsets);
    } catch (err: any) {
      if (err?.status === 404) {
        alignError = 'Route is not running.';
      } else if (err?.status === 400) {
        alignError = err?.message || 'Invalid offsets (each must be 0\u20131000 ms).';
      } else if (err?.status === 501) {
        alignDisabled = true;
        alignDisabledReason =
          'This route has no playout-side delay stage here (e.g. send-only), so alignment cannot be applied.';
        alignError = null;
      } else {
        alignError = err?.message || 'Failed to apply alignment.';
      }
    }
  }

  function resetAlignment() {
    alignOffsets = new Array(channelCount).fill(0);
    void applyAlignment();
  }

  // ------------------------------------------------------------------
  // Helpers
  // ------------------------------------------------------------------
  function timeAgo(iso?: string): string {
    if (!iso) return '\u2014';
    const t = new Date(iso).getTime();
    if (Number.isNaN(t)) return '\u2014';
    const s = Math.max(0, (Date.now() - t) / 1000);
    if (s < 60) return `${s.toFixed(0)}s ago`;
    if (s < 3600) return `${(s / 60).toFixed(0)}m ago`;
    return `${(s / 3600).toFixed(1)}h ago`;
  }

  // ------------------------------------------------------------------
  // Lifecycle: poll status while open, refresh alignment once
  // ------------------------------------------------------------------
  let pollTimer: ReturnType<typeof setInterval> | null = null;

  onMount(() => {
    void loadStatus(routeId);
    void loadAlignment(routeId);
    pollTimer = setInterval(() => void loadStatus(routeId), 1500);
  });

  onDestroy(() => {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  });
</script>

<!-- Backdrop + right-side drawer (sits below the app header) -->
<div class="fixed left-0 right-0 bottom-0 top-14 z-40" role="dialog" aria-modal="true" aria-label="Route inspector">
  <div class="absolute inset-0 bg-black/50" onclick={() => matrixStore.closeInspector()} aria-hidden="true"></div>

  <aside
    class="absolute top-0 right-0 h-full w-full max-w-md bg-zinc-950 border-l border-zinc-800 shadow-2xl flex flex-col animate-in slide-in-from-right duration-200"
  >
    <!-- Header -->
    <div class="flex items-start justify-between border-b border-zinc-800 px-4 py-3 shrink-0">
      <div class="min-w-0">
        <div class="flex items-center gap-2">
          <Gauge class="w-4 h-4 text-cyan-400 shrink-0" />
          <h2 class="text-sm font-bold font-mono text-zinc-100 truncate">{routeId}</h2>
        </div>
        <div class="mt-1 flex items-center gap-2 text-[11px] font-mono text-zinc-400">
          <span class="truncate">{srcLabel} &rarr; {dstLabel}</span>
          {#if status}
            <span class="rounded bg-zinc-800 px-1.5 py-0.5 text-[9px] uppercase tracking-wide border border-zinc-700 shrink-0">
              {status.direction}
            </span>
          {/if}
        </div>
      </div>
      <button
        onclick={() => matrixStore.closeInspector()}
        class="rounded p-1 text-zinc-400 hover:text-zinc-100 transition-colors"
        aria-label="Close inspector"
      >
        <X class="w-4 h-4" />
      </button>
    </div>

    <!-- Scrollable body -->
    <div class="flex-1 overflow-y-auto">
      <!-- ========================================================== -->
      <!-- 1. STATUS — "why isn't this playing"                       -->
      <!-- ========================================================== -->
      <section>
        <div class="flex items-center justify-between px-4 pt-3 pb-1">
          <h3 class="text-[10px] font-mono font-bold uppercase tracking-wider text-zinc-400">
            Why isn't this playing?
          </h3>
          {#if status}
            <span
              class="flex items-center gap-1.5 text-[10px] font-mono font-bold {status.playing
                ? 'text-emerald-400'
                : 'text-red-400'}"
            >
              <span class="h-2 w-2 rounded-full {status.playing ? 'bg-emerald-400' : 'bg-red-400'}"></span>
              {status.playing ? 'PLAYING' : 'NOT PLAYING'}
            </span>
          {:else if routeNotFound}
            <span class="text-[10px] font-mono font-bold text-amber-400">NOT RUNNING</span>
          {/if}
        </div>

        {#if routeNotFound}
          <p class="px-4 py-3 text-xs font-mono text-amber-300">
            Route is not running &mdash; it may have been disconnected. Re-patch the crosspoint to start it.
          </p>
        {:else if statusError}
          <p class="px-4 py-3 text-xs font-mono text-amber-300">{statusError}</p>
        {:else if !status}
          <p class="px-4 py-3 text-xs font-mono text-zinc-500 flex items-center gap-2">
            <RefreshCw class="w-3.5 h-3.5 animate-spin text-cyan-400" /> Loading status&hellip;
          </p>
        {:else}
          <div class="px-4 py-3 space-y-3">
            <p class="text-xs text-zinc-200">{status.summary}</p>

            {#if status.reasons && status.reasons.length}
              <ul class="space-y-1.5">
                {#each status.reasons as reason}
                  <li class="flex items-start gap-1.5 text-xs font-mono text-amber-300">
                    <AlertCircle class="w-3.5 h-3.5 text-amber-400 shrink-0 mt-0.5" />
                    <span>{reason}</span>
                  </li>
                {/each}
              </ul>
            {:else if status.playing}
              <p class="flex items-center gap-1.5 text-xs font-mono text-emerald-400">
                <CheckCircle class="w-3.5 h-3.5" /> No issues detected.
              </p>
            {/if}

            <!-- Counters grid -->
            <div class="grid grid-cols-2 gap-2 text-[11px] font-mono">
              <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                <span class="text-[10px] text-zinc-500 block">PACKETS RX</span>
                <span class="text-zinc-100 font-bold">{status.packets_received.toLocaleString()}</span>
              </div>
              <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                <span class="text-[10px] text-zinc-500 block">JITTER DEPTH</span>
                <span class="text-cyan-300 font-bold">{status.jitter_depth_frames} fr</span>
              </div>
              <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                <span class="text-[10px] text-zinc-500 block">UNDERRUNS</span>
                <span class="{status.underruns > 0 ? 'text-amber-300' : 'text-zinc-300'} font-bold">{status.underruns}</span>
              </div>
              <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                <span class="text-[10px] text-zinc-500 block">OVERRUNS</span>
                <span class="{status.overruns > 0 ? 'text-amber-300' : 'text-zinc-300'} font-bold">{status.overruns}</span>
              </div>
              <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                <span class="text-[10px] text-zinc-500 block">DTX STATE</span>
                <span class="text-zinc-100 font-bold">{status.dtx_state || '\u2014'}</span>
              </div>
              <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                <span class="text-[10px] text-zinc-500 block">CAP / OUT OVF</span>
                <span class="{status.capture_overflows + status.output_underflows > 0 ? 'text-amber-300' : 'text-zinc-300'} font-bold">
                  {status.capture_overflows} / {status.output_underflows}
                </span>
              </div>
              <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                <span class="text-[10px] text-zinc-500 block">LAST INPUT</span>
                <span class="text-zinc-100 font-bold">{timeAgo(status.last_input)}</span>
              </div>
              <div class="rounded-lg bg-zinc-950/70 p-2 border border-zinc-800/60">
                <span class="text-[10px] text-zinc-500 block">LAST PLAYOUT</span>
                <span class="text-zinc-100 font-bold">{timeAgo(status.last_playout)}</span>
              </div>
            </div>

            <!-- Per-peer link table -->
            {#if status.peers && status.peers.length}
              <div>
                <div class="text-[10px] font-mono font-bold uppercase tracking-wider text-zinc-400 mb-1.5">Peers</div>
                <div class="rounded-lg border border-zinc-800/80 overflow-hidden">
                  <table class="w-full text-[11px] font-mono">
                    <thead class="bg-zinc-950/70 text-zinc-500 text-[9px] uppercase">
                      <tr>
                        <th class="px-2 py-1 text-left">Peer</th>
                        <th class="px-2 py-1 text-left">Role</th>
                        <th class="px-2 py-1 text-right">Sent</th>
                        <th class="px-2 py-1 text-right">Lost</th>
                        <th class="px-2 py-1 text-right">Jit fr</th>
                      </tr>
                    </thead>
                    <tbody class="divide-y divide-zinc-800/60 text-zinc-200">
                      {#each status.peers as peer}
                        <tr>
                          <td class="px-2 py-1 text-zinc-100" title={peer.address}>{peer.peer_id || peer.address}</td>
                          <td class="px-2 py-1 text-zinc-400 uppercase">{peer.role}</td>
                          <td class="px-2 py-1 text-right text-zinc-300">{peer.packets_sent.toLocaleString()}</td>
                          <td class="px-2 py-1 text-right {peer.packets_lost > 0 ? 'text-amber-300' : 'text-zinc-500'}">
                            {peer.packets_lost}
                          </td>
                          <td class="px-2 py-1 text-right text-cyan-300">{peer.jitter_depth_frames}</td>
                        </tr>
                      {/each}
                    </tbody>
                  </table>
                </div>
              </div>
            {/if}
          </div>
        {/if}
      </section>

      <div class="border-t border-zinc-800"></div>

      <!-- ========================================================== -->
      <!-- 2. CALIBRATION                                             -->
      <!-- ========================================================== -->
      <section class="py-3">
        <h3 class="text-[10px] font-mono font-bold uppercase tracking-wider text-zinc-400 px-4 pb-2">
          Calibration
        </h3>

        {#if calibrating}
          <div class="flex items-center gap-2 text-xs font-mono text-cyan-300 px-4 py-2">
            <RefreshCw class="w-4 h-4 animate-spin" />
            <span>Calibrating&hellip; injecting probe tone (~1 s)</span>
          </div>
        {:else}
          <div class="flex items-center gap-2 px-4">
            <button
              onclick={() => doCalibrate(false)}
              class="inline-flex items-center gap-1.5 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 py-1 text-xs font-medium text-zinc-200 hover:bg-zinc-800 hover:text-cyan-300 transition-colors"
              title="Measure per-channel pipeline delay (does not change the route)"
            >
              <Activity class="w-3.5 h-3.5 text-cyan-400" />
              <span>Calibrate</span>
            </button>
            <button
              onclick={() => doCalibrate(true)}
              class="inline-flex items-center gap-1.5 rounded-md bg-cyan-500 px-2.5 py-1 text-xs font-semibold text-zinc-950 hover:bg-cyan-400 transition-colors"
              title="Measure and store offsets (max &minus; delay) as the route alignment"
            >
              <span>Calibrate &amp; Apply</span>
            </button>
            <button
              onclick={() => (showAdvanced = !showAdvanced)}
              class="ml-auto text-[10px] font-mono text-zinc-500 hover:text-zinc-300 transition-colors"
            >
              {showAdvanced ? '\u25be' : '\u25b8'} advanced
            </button>
          </div>

          {#if showAdvanced}
            <div class="px-4 pt-2 grid grid-cols-3 gap-2">
              <label class="flex flex-col gap-1 text-[10px] font-mono text-zinc-400">
                Freq (Hz)
                <input
                  type="number" min="100" max="20000" step="100"
                  bind:value={probeFreq}
                  class="rounded border border-zinc-800 bg-zinc-900 px-2 py-1 text-zinc-100 focus:border-cyan-500 focus:outline-none"
                />
              </label>
              <label class="flex flex-col gap-1 text-[10px] font-mono text-zinc-400">
                Amp
                <input
                  type="number" min="1000" max="32000" step="1000"
                  bind:value={probeAmp}
                  class="rounded border border-zinc-800 bg-zinc-900 px-2 py-1 text-zinc-100 focus:border-cyan-500 focus:outline-none"
                />
              </label>
              <label class="flex flex-col gap-1 text-[10px] font-mono text-zinc-400">
                Dur (ms)
                <input
                  type="number" min="100" max="5000" step="100"
                  bind:value={probeDur}
                  class="rounded border border-zinc-800 bg-zinc-900 px-2 py-1 text-zinc-100 focus:border-cyan-500 focus:outline-none"
                />
              </label>
              <span class="col-span-3 text-[10px] font-mono text-zinc-500">
                Leave blank to use defaults (4200 Hz, 16000, 600 ms).
              </span>
            </div>
          {/if}
        {/if}

        {#if calibrateError}
          <p class="px-4 pt-2 text-xs font-mono text-red-400 flex items-start gap-1.5">
            <AlertCircle class="w-3.5 h-3.5 shrink-0 mt-0.5" />
            <span>{calibrateError}</span>
          </p>
        {/if}

        {#if calibrateResult}
          {#if calibrateResult.detected}
            <div class="px-4 pt-2 text-[11px] font-mono space-y-1.5">
              <p class="text-emerald-400 flex items-center gap-1.5">
                <CheckCircle class="w-3.5 h-3.5" />
                Detected{calibrateResult.applied ? ' \u00b7 offsets applied' : ''}
                {#if calibrateResult.delay_ms !== undefined}
                  <span class="text-emerald-300/80">&middot; avg {calibrateResult.delay_ms.toFixed(2)} ms</span>
                {/if}
              </p>
              {#if calibrateResult.channel_delays_ms && calibrateResult.channel_delays_ms.length}
                <div class="flex flex-wrap gap-1.5">
                  {#each calibrateResult.channel_delays_ms as d, i}
                    <span class="rounded bg-zinc-900 border border-zinc-800 px-1.5 py-0.5 text-zinc-200">
                      ch{i} {d.toFixed(2)} ms
                    </span>
                  {/each}
                </div>
              {/if}
              {#if calibrateResult.offsets_ms && calibrateResult.offsets_ms.length}
                <p class="text-zinc-400">
                  offsets: {calibrateResult.offsets_ms.map((o) => o.toFixed(1)).join(', ')} ms
                </p>
              {/if}
            </div>
          {:else}
            <p class="px-4 pt-2 text-xs font-mono text-amber-300 flex items-start gap-1.5">
              <AlertCircle class="w-3.5 h-3.5 shrink-0 mt-0.5" />
              <span>{calibrateResult.note || 'Probe tone was not detected in time.'}</span>
            </p>
          {/if}
        {/if}
      </section>

      <div class="border-t border-zinc-800"></div>

      <!-- ========================================================== -->
      <!-- 3. ALIGNMENT                                               -->
      <!-- ========================================================== -->
      <section class="py-3">
        <h3 class="text-[10px] font-mono font-bold uppercase tracking-wider text-zinc-400 px-4 pb-2">
          Alignment (per-channel offset, ms)
        </h3>

        {#if alignDisabled}
          <p class="px-4 py-2 text-xs font-mono text-amber-300">{alignDisabledReason}</p>
        {:else}
          <div class="px-4 space-y-2">
            {#each alignOffsets as off, i}
              <label class="flex items-center gap-2 text-[11px] font-mono">
                <span class="w-8 text-zinc-400 shrink-0">ch{i}</span>
                <input
                  type="number" min="0" max="1000" step="0.1"
                  bind:value={alignOffsets[i]}
                  class="w-24 rounded border border-zinc-800 bg-zinc-900 px-2 py-1 text-zinc-100 focus:border-cyan-500 focus:outline-none"
                />
                <span class="text-zinc-500">ms delay</span>
              </label>
            {:else}
              <p class="text-xs font-mono text-zinc-500">No channels reported for this route.</p>
            {/each}

            <div class="flex items-center gap-2 pt-1">
              <button
                onclick={() => applyAlignment()}
                class="inline-flex items-center gap-1.5 rounded-md bg-cyan-500 px-2.5 py-1 text-xs font-semibold text-zinc-950 hover:bg-cyan-400 transition-colors"
                title="Apply these offsets live (0&ndash;1000 ms each)"
              >
                <span>Apply</span>
              </button>
              <button
                onclick={() => resetAlignment()}
                class="inline-flex items-center gap-1.5 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 py-1 text-xs font-medium text-zinc-300 hover:bg-zinc-800 transition-colors"
                title="Set all offsets to 0 and apply"
              >
                <span>Reset</span>
              </button>
            </div>

            {#if alignError}
              <p class="text-xs font-mono text-red-400 flex items-start gap-1.5">
                <AlertCircle class="w-3.5 h-3.5 shrink-0 mt-0.5" />
                <span>{alignError}</span>
              </p>
            {/if}
            <p class="text-[10px] font-mono text-zinc-500">
              Applied values are quantized to whole sample periods by the daemon.
            </p>
          </div>
        {/if}
      </section>
    </div>
  </aside>
</div>
