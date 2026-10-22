<script lang="ts">
  import { onMount } from 'svelte';
  import { apiClient } from '$lib/api/client';
  import type { SimProfile, SimProfiles } from '$lib/api/types';
  import { SIM_DIRECTIONS, emptySimProfile } from '$lib/api/types';
  import { Switch } from 'bits-ui';
  import FlaskConical from '@lucide/svelte/icons/flask-conical';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import AlertCircle from '@lucide/svelte/icons/alert-circle';
  import CheckCircle from '@lucide/svelte/icons/check-circle';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';

  type FieldKey =
    | 'loss_pct'
    | 'latency_ms'
    | 'jitter_ms'
    | 'reorder_pct'
    | 'duplicate_pct'
    | 'bandwidth_kbps';

  const FIELDS: { key: FieldKey; label: string; unit: string; min: number; max: number; step: number }[] = [
    { key: 'loss_pct', label: 'Loss', unit: '%', min: 0, max: 100, step: 1 },
    { key: 'latency_ms', label: 'Latency', unit: 'ms', min: 0, max: 5000, step: 5 },
    { key: 'jitter_ms', label: 'Jitter', unit: 'ms', min: 0, max: 5000, step: 5 },
    { key: 'reorder_pct', label: 'Reorder', unit: '%', min: 0, max: 100, step: 1 },
    { key: 'duplicate_pct', label: 'Duplicate', unit: '%', min: 0, max: 100, step: 1 },
    { key: 'bandwidth_kbps', label: 'Bandwidth', unit: 'kbps', min: 0, max: 100000, step: 64 },
  ];

  let profiles = $state<SimProfiles>({ in: emptySimProfile(), out: emptySimProfile() });
  let unavailable = $state(false);
  let loadError = $state<string | null>(null);
  let applying = $state(false);
  let appliedAt = $state<string | null>(null);
  let applyError = $state<string | null>(null);

  onMount(async () => {
    try {
      profiles = await apiClient.getNetworkSim();
    } catch (err: any) {
      if (err?.status === 501) {
        unavailable = true;
      } else {
        loadError = err?.message || 'Failed to load the simulation profile.';
      }
    }
  });

  function clampPct(v: number): number {
    if (Number.isNaN(v)) return 0;
    return Math.min(100, Math.max(0, v));
  }
  function clampNonNeg(v: number): number {
    if (Number.isNaN(v)) return 0;
    return Math.max(0, v);
  }

  function buildPayload(): SimProfiles {
    const clamp = (p: SimProfile): SimProfile => ({
      enabled: p.enabled,
      loss_pct: clampPct(p.loss_pct),
      latency_ms: clampNonNeg(p.latency_ms),
      jitter_ms: clampNonNeg(p.jitter_ms),
      reorder_pct: clampPct(p.reorder_pct),
      duplicate_pct: clampPct(p.duplicate_pct),
      bandwidth_kbps: clampNonNeg(p.bandwidth_kbps),
    });
    return { in: clamp(profiles.in), out: clamp(profiles.out) };
  }

  async function apply() {
    if (applying) return;
    applying = true;
    applyError = null;
    try {
      profiles = await apiClient.setNetworkSim(buildPayload());
      appliedAt = new Date().toLocaleTimeString();
    } catch (err: any) {
      if (err?.status === 501) {
        unavailable = true;
        applyError = 'Simulation is not available on this daemon (UDP mode only).';
      } else {
        applyError = err?.message || 'Failed to apply the simulation profile.';
      }
    } finally {
      applying = false;
    }
  }

  function resetAll() {
    profiles = { in: emptySimProfile(), out: emptySimProfile() };
    void apply();
  }
</script>

<div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-5 shadow-xl">
  <!-- Panel Header -->
  <div class="flex flex-col sm:flex-row sm:items-center justify-between border-b border-zinc-800 pb-3 mb-4 gap-2">
    <div class="flex items-center gap-2">
      <FlaskConical class="w-4 h-4 text-amber-400" />
      <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
        Network Condition Simulator
      </h2>
      <span class="rounded bg-amber-500/10 px-2 py-0.5 text-[10px] font-mono font-semibold text-amber-300 border border-amber-500/30">
        LOCAL QA &middot; UDP PATH ONLY
      </span>
    </div>
    <span class="text-[11px] font-mono text-zinc-500">
      Degrade this node&rsquo;s UDP audio I/O to exercise jitter buffer, FEC &amp; PLC
    </span>
  </div>

  {#if unavailable}
    <div class="rounded-lg border border-amber-500/30 bg-amber-950/30 p-3 text-xs font-mono text-amber-300 flex items-start gap-2">
      <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
      <span>
        Network simulation is not available on this daemon. It only applies to the UDP audio path
        (read-only or DTLS/encrypt mode cannot be simulated, since those are reliable streams).
      </span>
    </div>
  {:else}
    {#if loadError}
      <div class="mb-4 rounded-lg border border-red-500/30 bg-red-950/30 p-3 text-xs font-mono text-red-300 flex items-start gap-2">
        <AlertCircle class="w-4 h-4 shrink-0 mt-0.5" />
        <span>{loadError}</span>
      </div>
    {/if}

    <!-- Two direction columns -->
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
      {#each SIM_DIRECTIONS as dir}
        {@const prof = profiles[dir.key]}
        <div class="rounded-lg border border-zinc-800/80 bg-zinc-950/50 p-3.5 space-y-2.5">
          <!-- Direction header + enable toggle -->
          <div class="flex items-center justify-between border-b border-zinc-800/70 pb-2">
            <span class="text-[10px] font-mono font-bold uppercase tracking-wider {prof.enabled ? 'text-amber-300' : 'text-zinc-400'}">
              {dir.label}
            </span>
            <Switch.Root
              checked={prof.enabled}
              onCheckedChange={(checked) => (profiles[dir.key].enabled = checked)}
              class="relative inline-flex h-5 w-10 shrink-0 cursor-pointer rounded-full border border-zinc-700 bg-zinc-800 transition-colors duration-200 ease-in-out focus:outline-none data-[state=checked]:bg-amber-500"
              aria-label="Enable simulation for {dir.label}"
            >
              <Switch.Thumb
                class="pointer-events-none inline-block h-4 w-4 transform rounded-full bg-zinc-200 shadow-md transition duration-200 ease-in-out data-[state=checked]:translate-x-5 data-[state=checked]:bg-zinc-950 data-[state=unchecked]:translate-x-0.5"
              />
            </Switch.Root>
          </div>

          <!-- Condition fields -->
          <div class="grid grid-cols-2 gap-2.5">
            {#each FIELDS as f}
              <label class="flex flex-col gap-1 text-[10px] font-mono">
                <span class="text-zinc-400 flex items-center justify-between">
                  {f.label}
                  <span class="text-zinc-600">{f.unit}{f.key === 'bandwidth_kbps' ? ' (0=\u221e)' : ''}</span>
                </span>
                <input
                  type="number"
                  min={f.min}
                  max={f.max}
                  step={f.step}
                  bind:value={profiles[dir.key][f.key]}
                  disabled={!prof.enabled}
                  class="rounded border border-zinc-800 bg-zinc-900 px-2 py-1 text-zinc-100 focus:border-amber-500 focus:outline-none disabled:opacity-40 disabled:cursor-not-allowed"
                />
              </label>
            {/each}
          </div>
        </div>
      {/each}
    </div>

    <!-- Actions -->
    <div class="mt-4 flex flex-wrap items-center gap-2 border-t border-zinc-800 pt-4">
      <button
        onclick={() => apply()}
        disabled={applying}
        class="inline-flex items-center gap-1.5 rounded-md bg-amber-500 px-3 py-1.5 text-xs font-semibold text-zinc-950 hover:bg-amber-400 transition-colors disabled:opacity-50"
        title="Apply these conditions to the local UDP audio path"
      >
        {#if applying}
          <RefreshCw class="w-3.5 h-3.5 animate-spin" />
          <span>Applying&hellip;</span>
        {:else}
          <span>Apply</span>
        {/if}
      </button>

      <button
        onclick={() => resetAll()}
        disabled={applying}
        class="inline-flex items-center gap-1.5 rounded-md border border-zinc-800 bg-zinc-900 px-3 py-1.5 text-xs font-medium text-zinc-300 hover:bg-zinc-800 transition-colors disabled:opacity-50"
        title="Disable both directions (all values zero) and apply"
      >
        <RotateCcw class="w-3.5 h-3.5 text-zinc-400" />
        <span>Reset All</span>
      </button>

      {#if applyError}
        <span class="text-xs font-mono text-red-400 flex items-center gap-1.5">
          <AlertCircle class="w-3.5 h-3.5" /> {applyError}
        </span>
      {:else if appliedAt}
        <span class="text-xs font-mono text-emerald-400 flex items-center gap-1.5">
          <CheckCircle class="w-3.5 h-3.5" /> Applied &middot; {appliedAt}
        </span>
      {/if}
    </div>
  {/if}
</div>
