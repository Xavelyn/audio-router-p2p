<script lang="ts">
  import type { DeviceHealth, RouteHealth } from '$lib/api/types';
  import { telemetryStore } from '$lib/stores/telemetry.svelte';
  import {
    SEVERITY_ORDER,
    formatRate,
    formatScore,
    isTelemetryFresh,
    severityBadgeClass,
    severityDotClass,
    severityLabel,
    worstPeer,
  } from '$lib/utils/artifacts';
  import Activity from '@lucide/svelte/icons/activity';
  import AudioLines from '@lucide/svelte/icons/audio-lines';

  interface RouteRow {
    routeId: string;
    health: RouteHealth;
  }

  interface DeviceRow {
    urn: string;
    health: DeviceHealth;
  }

  // Per-route rows: only entries carrying the optional `health` field
  // (aggregator wired). Deterministic order by route ID.
  const routeRows = $derived.by<RouteRow[]>(() => {
    const rows: RouteRow[] = [];
    for (const [routeId, st] of Object.entries(telemetryStore.streamStats)) {
      if (st.health) rows.push({ routeId, health: st.health });
    }
    return rows.sort((a, b) => a.routeId.localeCompare(b.routeId));
  });

  // Device rows sourced from the URN-keyed endpoint (ADR 0034 §4).
  const deviceRows = $derived.by<DeviceRow[]>(() => {
    const rows: DeviceRow[] = [];
    for (const [urn, dh] of Object.entries(telemetryStore.deviceArtifacts)) {
      if (dh) rows.push({ urn, health: dh });
    }
    return rows.sort((a, b) => a.urn.localeCompare(b.urn));
  });

  // Distinguish "no data" (aggregator unwired / stale telemetry) from
  // "all clean": the panel shows an explicit empty state, never green.
  const hasData = $derived(
    isTelemetryFresh(telemetryStore.lastUpdated, Date.now()) &&
      (routeRows.length > 0 || deviceRows.length > 0)
  );

  const legendTiers = $derived(
    (Object.keys(SEVERITY_ORDER) as Array<'NONE' | 'MINOR' | 'MAJOR' | 'CRITICAL'>)
      .slice()
      .sort((a, b) => SEVERITY_ORDER[a] - SEVERITY_ORDER[b])
  );

  // Compact windowed-counter chips for a route row: [label, count, rate|null].
  // `emerald` marks the informational FEC-recovered chip (proof FEC works,
  // not a defect); `hot` marks nonzero artifact counters.
  function routeChips(h: RouteHealth): Array<{
    label: string;
    count: number;
    rate: number | null;
    cls: string;
  }> {
    const neutral = 'border-zinc-800 bg-zinc-950/70 text-zinc-300';
    const hot = 'border-amber-500/40 bg-amber-500/10 text-amber-300';
    const emerald = 'border-emerald-500/30 bg-emerald-500/10 text-emerald-300';
    return [
      { label: 'U', count: h.underruns, rate: h.rates?.underrun_per_s ?? null, cls: h.underruns > 0 ? hot : neutral },
      { label: 'O', count: h.overruns, rate: h.rates?.overrun_per_s ?? null, cls: h.overruns > 0 ? hot : neutral },
      { label: 'RD', count: h.ring_drops, rate: h.rates?.ring_drop_per_s ?? null, cls: h.ring_drops > 0 ? hot : neutral },
      { label: 'PLC', count: h.plc_concealed, rate: null, cls: h.plc_concealed > 0 ? hot : neutral },
      { label: 'FEC!', count: h.fec_unrecoverable, rate: null, cls: h.fec_unrecoverable > 0 ? hot : neutral },
      { label: 'LOSS', count: h.packets_lost, rate: null, cls: h.packets_lost > 0 ? hot : neutral },
      { label: 'FEC✓', count: h.fec_recovered, rate: null, cls: h.fec_recovered > 0 ? emerald : neutral },
    ];
  }

  function scoreColorClass(score: number): string {
    if (score >= 90) return 'text-emerald-400';
    if (score >= 60) return 'text-amber-400';
    if (score >= 30) return 'text-orange-400';
    return 'text-red-400';
  }
</script>

<section class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-5 shadow-xl">
  <!-- Panel Header -->
  <div class="flex flex-col sm:flex-row sm:items-center justify-between border-b border-zinc-800 pb-3 mb-4 gap-2">
    <div class="flex items-center gap-2 min-w-0">
      <AudioLines class="w-4 h-4 text-cyan-400 shrink-0" />
      <h3 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
        Audio Artifacts (Per-Route &amp; Per-Device)
      </h3>
    </div>

    <div class="flex items-center gap-3">
      <!-- 4-tier ArtifactSeverity legend (NOT the 5-tier NodeHealthGrade) -->
      <div class="flex items-center gap-2.5 text-[10px] font-mono">
        {#each legendTiers as tier (tier)}
          <div class="flex items-center gap-1">
            <span class="h-2 w-2 rounded {severityDotClass(tier)}"></span>
            <span class="text-zinc-400">{severityLabel(tier)}</span>
          </div>
        {/each}
      </div>
      <span class="text-[10px] font-mono text-zinc-500 hidden md:inline shrink-0">
        10 s windowed &bull; ADR 0034
      </span>
    </div>
  </div>

  {#if !hasData}
    <div class="rounded-lg border border-dashed border-zinc-800 p-6 text-center text-xs font-mono text-zinc-500">
      No artifact data — the daemon's artifact aggregator is unwired or telemetry is stale
      (<code class="text-zinc-400">health</code>/<code class="text-zinc-400">devices</code> omitted from stream-stats).
      This is "no data", not "clean".
    </div>
  {:else}
    <div class="space-y-5">
      <!-- ==================================================================== -->
      <!-- Per-Route Table                                                       -->
      <!-- ==================================================================== -->
      <div>
        <div class="flex items-center gap-2 mb-2 text-[10px] font-mono font-semibold uppercase tracking-wider text-zinc-400">
          <Activity class="w-3 h-3 text-cyan-400" />
          <span>Route Paths ({routeRows.length})</span>
        </div>

        {#if routeRows.length === 0}
          <div class="rounded-lg border border-dashed border-zinc-800 p-4 text-center text-[11px] font-mono text-zinc-500">
            No live routes report artifact health (aggregator wired, no routes with
            <code class="text-zinc-400">health</code>).
          </div>
        {:else}
          <div class="overflow-x-auto rounded-lg border border-zinc-800/80 bg-zinc-950/40 shadow-inner">
            <table class="w-full text-left text-xs font-mono">
              <thead class="border-b border-zinc-800 bg-zinc-950/80 text-zinc-400 text-[10px] uppercase">
                <tr>
                  <th class="px-3 py-2.5 whitespace-nowrap">Route ID</th>
                  <th class="px-3 py-2.5 whitespace-nowrap">Health</th>
                  <th class="px-3 py-2.5 whitespace-nowrap">Windowed 10 s Counters (e/s)</th>
                  <th class="px-3 py-2.5 whitespace-nowrap">Worst Peer</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-zinc-800/60 text-zinc-200">
                {#each routeRows as row (row.routeId)}
                  {@const h = row.health}
                  {@const wp = worstPeer(h)}
                  {@const chips = routeChips(h)}
                  <tr class="hover:bg-zinc-800/30 transition-colors align-top">
                    <!-- Route ID + ghost flag -->
                    <td class="px-3 py-2.5 min-w-[160px] max-w-[280px]">
                      <div class="flex items-center gap-1.5 min-w-0">
                        {#if h.ghost}
                          <span class="rounded bg-amber-500/20 px-1 py-px text-[9px] font-bold uppercase text-amber-300 border border-amber-500/40 shrink-0">
                            GHOST
                          </span>
                        {/if}
                        <span class="truncate text-zinc-300" title={row.routeId}>{row.routeId}</span>
                      </div>
                    </td>

                    <!-- Pill + score -->
                    <td class="px-3 py-2.5 whitespace-nowrap">
                      <div class="flex items-center gap-2">
                        <span
                          class="inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[9px] font-mono font-bold uppercase tracking-wider {severityBadgeClass(h.severity)}
                            {h.severity === 'CRITICAL' ? 'animate-pulse' : ''}"
                        >
                          <span class="h-1.5 w-1.5 rounded-full {severityDotClass(h.severity)}" aria-hidden="true"></span>
                          {severityLabel(h.severity)}
                        </span>
                        <span class="text-sm font-bold {scoreColorClass(h.score)}">
                          {formatScore(h.score)}
                        </span>
                      </div>
                    </td>

                    <!-- Windowed counter chips with e/s rates -->
                    <td class="px-3 py-2.5">
                      <div class="flex flex-wrap gap-1.5 max-w-[520px]">
                        {#each chips as chip (chip.label)}
                          <span
                            class="inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[10px] font-mono {chip.cls}"
                            title="{chip.label} windowed count{chip.rate !== null ? ` at ${formatRate(chip.rate)}/s` : ''}"
                          >
                            <span class="font-semibold uppercase opacity-70">{chip.label}</span>
                            <span class="font-bold">{chip.count}</span>
                            {#if chip.rate !== null}
                              <span class="opacity-60">{formatRate(chip.rate)}</span>
                            {/if}
                          </span>
                        {/each}
                      </div>
                    </td>

                    <!-- Worst peer: max underruns+overruns+late_packets, ties by address -->
                    <td class="px-3 py-2.5 whitespace-nowrap">
                      {#if wp}
                        <div class="flex items-center gap-1.5" title="{wp.addr}: U{wp.underruns} O{wp.overruns} late {wp.late_packets}">
                          <span class="truncate max-w-[180px] text-zinc-300" title={wp.addr}>{wp.addr}</span>
                          <span class="text-[10px] text-zinc-500">
                            U{wp.underruns} O{wp.overruns} L{wp.late_packets}
                          </span>
                        </div>
                      {:else}
                        <span class="text-zinc-600">&mdash;</span>
                      {/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
      </div>

      <!-- ==================================================================== -->
      <!-- Per-Device Table (URN-keyed endpoint)                                 -->
      <!-- ==================================================================== -->
      <div>
        <div class="flex items-center gap-2 mb-2 text-[10px] font-mono font-semibold uppercase tracking-wider text-zinc-400">
          <AudioLines class="w-3 h-3 text-cyan-400" />
          <span>Physical Devices ({deviceRows.length})</span>
        </div>

        {#if deviceRows.length === 0}
          <div class="rounded-lg border border-dashed border-zinc-800 p-4 text-center text-[11px] font-mono text-zinc-500">
            No devices report artifact health.
          </div>
        {:else}
          <div class="overflow-x-auto rounded-lg border border-zinc-800/80 bg-zinc-950/40 shadow-inner">
            <table class="w-full text-left text-xs font-mono">
              <thead class="border-b border-zinc-800 bg-zinc-950/80 text-zinc-400 text-[10px] uppercase">
                <tr>
                  <th class="px-3 py-2.5 whitespace-nowrap">Device URN</th>
                  <th class="px-3 py-2.5 whitespace-nowrap">Health</th>
                  <th class="px-3 py-2.5 whitespace-nowrap">Input XRun (window/total)</th>
                  <th class="px-3 py-2.5 whitespace-nowrap">Output XRun (window/total)</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-zinc-800/60 text-zinc-200">
                {#each deviceRows as row (row.urn)}
                  {@const h = row.health}
                  <tr class="hover:bg-zinc-800/30 transition-colors">
                    <td class="px-3 py-2.5 min-w-[180px] max-w-[320px]">
                      <div class="flex items-center gap-1.5 min-w-0">
                        {#if h.ghost}
                          <span class="rounded bg-amber-500/20 px-1 py-px text-[9px] font-bold uppercase text-amber-300 border border-amber-500/40 shrink-0">
                            GHOST
                          </span>
                        {/if}
                        <span class="truncate text-zinc-300" title={h.urn || row.urn}>{h.urn || row.urn}</span>
                      </div>
                    </td>

                    <td class="px-3 py-2.5 whitespace-nowrap">
                      <div class="flex items-center gap-2">
                        <span
                          class="inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[9px] font-mono font-bold uppercase tracking-wider {severityBadgeClass(h.severity)}
                            {h.severity === 'CRITICAL' ? 'animate-pulse' : ''}"
                        >
                          <span class="h-1.5 w-1.5 rounded-full {severityDotClass(h.severity)}" aria-hidden="true"></span>
                          {severityLabel(h.severity)}
                        </span>
                        <span class="text-sm font-bold {scoreColorClass(h.score)}">
                          {formatScore(h.score)}
                        </span>
                      </div>
                    </td>

                    <td class="px-3 py-2.5 whitespace-nowrap">
                      {#if h.input}
                        <span class={h.input.xrun_window > 0 ? 'text-amber-300 font-bold' : 'text-zinc-300'}>
                          {h.input.xrun_window} / {h.input.xrun_total}
                        </span>
                      {:else}
                        <span class="text-zinc-600">&mdash;</span>
                      {/if}
                    </td>

                    <td class="px-3 py-2.5 whitespace-nowrap">
                      {#if h.output}
                        <span class={h.output.xrun_window > 0 ? 'text-amber-300 font-bold' : 'text-zinc-300'}>
                          {h.output.xrun_window} / {h.output.xrun_total}
                        </span>
                      {:else}
                        <span class="text-zinc-600">&mdash;</span>
                      {/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
      </div>

      <!-- Attribution note (ADR 0034 §1.C) -->
      <p class="text-[10px] font-mono text-zinc-500 leading-relaxed">
        Device XRuns are shared across every route using the physical device and are
        <strong class="text-zinc-400">never folded into route scores</strong>. A route transiting a
        degraded device keeps its own (pure) score; check the device rows and the flow-canvas
        device pills to see the shared impact.
      </p>
    </div>
  {/if}
</section>
