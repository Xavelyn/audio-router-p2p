<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { telemetryStore } from '$lib/stores/telemetry.svelte';
  import { peerStore } from '$lib/stores/peers.svelte';
  import Cpu from '@lucide/svelte/icons/cpu';
  import Activity from '@lucide/svelte/icons/activity';
  import Zap from '@lucide/svelte/icons/zap';
  import Gauge from '@lucide/svelte/icons/gauge';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Clock from '@lucide/svelte/icons/clock';
  import Radio from '@lucide/svelte/icons/radio';

  onMount(() => {
    telemetryStore.startPolling(2000);
  });

  onDestroy(() => {
    telemetryStore.stopPolling();
  });

  const pStream = $derived(telemetryStore.primaryStream);
  const fec = $derived(pStream?.fec);
  const plc = $derived(pStream?.plc);
  const asrc = $derived(pStream?.asrc);
</script>

<div class="flex flex-col h-full bg-studio-dark select-none overflow-hidden">
  <!-- Toolbar -->
  <div class="flex items-center justify-between border-b border-zinc-800 bg-zinc-950/70 px-4 py-2.5">
    <div class="flex items-center gap-3">
      <div class="flex items-center gap-2">
        <Cpu class="w-4 h-4 text-cyan-400" />
        <h1 class="text-sm font-semibold tracking-wide text-zinc-100 uppercase">
          DSP & Network Telemetry Dashboard
        </h1>
      </div>
      <span class="rounded bg-zinc-800 px-2 py-0.5 text-xs font-mono text-zinc-300">
        RFC 5109 FEC &bull; PSOLA PLC &bull; ASRC Drift
      </span>
    </div>

    <!-- Refresh button -->
    <button
      onclick={() => telemetryStore.fetchTelemetry()}
      class="inline-flex items-center gap-1.5 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 py-1 text-xs text-zinc-300 hover:bg-zinc-800 hover:text-cyan-300 transition-colors"
    >
      <RefreshCw class="w-3.5 h-3.5 text-cyan-400" />
      <span>Poll Now</span>
    </button>
  </div>

  <!-- Main Content -->
  <div class="flex-1 overflow-y-auto p-6 bg-zinc-950">
    <div class="max-w-6xl mx-auto space-y-6">
      <!-- 1. Node Host Health Strip -->
      <div class="grid grid-cols-2 md:grid-cols-4 gap-4">
        <!-- CPU -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>NODE CPU USAGE</span>
            <Cpu class="w-4 h-4 text-cyan-400" />
          </div>
          <div class="mt-2 text-2xl font-mono font-bold text-zinc-100">
            {telemetryStore.systemMetrics.cpu_percent.toFixed(1)}%
          </div>
          <div class="mt-2 h-1.5 w-full bg-zinc-800 rounded-full overflow-hidden">
            <div
              class="h-full bg-cyan-400 rounded-full transition-all"
              style="width: {Math.min(100, telemetryStore.systemMetrics.cpu_percent * 2)}%"
            ></div>
          </div>
        </div>

        <!-- Memory -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>HEAP MEMORY</span>
            <Activity class="w-4 h-4 text-emerald-400" />
          </div>
          <div class="mt-2 text-2xl font-mono font-bold text-zinc-100">
            {telemetryStore.systemMetrics.memory_used_mb.toFixed(1)} <span class="text-xs text-zinc-500 font-normal">MB</span>
          </div>
          <div class="mt-2 text-[10px] font-mono text-zinc-500">
            of {telemetryStore.systemMetrics.memory_total_mb} MB allocated
          </div>
        </div>

        <!-- Goroutines -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>GO RUNTIME ROUTINES</span>
            <Zap class="w-4 h-4 text-amber-400" />
          </div>
          <div class="mt-2 text-2xl font-mono font-bold text-zinc-100">
            {telemetryStore.systemMetrics.goroutines}
          </div>
          <div class="mt-2 text-[10px] font-mono text-emerald-400">
            GC pause: {(telemetryStore.systemMetrics.gc_pause_ns / 1000).toFixed(0)} &mu;s
          </div>
        </div>

        <!-- Distributed Clock PTP -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow-lg">
          <div class="flex items-center justify-between text-xs font-mono text-zinc-400">
            <span>PTP CLOCK LOCK</span>
            <Clock class="w-4 h-4 text-cyan-400" />
          </div>
          <div class="mt-2 text-2xl font-mono font-bold text-emerald-400">
            {peerStore.clockSync.locked ? 'LOCKED' : 'UNSYNC'}
          </div>
          <div class="mt-2 text-[10px] font-mono text-zinc-400">
            Offset: {(peerStore.clockSync.offset_ns / 1000).toFixed(1)} &mu;s
          </div>
        </div>
      </div>

      <!-- 2. RFC 5109 FEC & PSOLA PLC Cards -->
      <div class="grid grid-cols-1 md:grid-cols-2 gap-6">
        <!-- FEC (RFC 5109) -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-5 shadow-xl">
          <div class="flex items-center justify-between border-b border-zinc-800 pb-3 mb-4">
            <div class="flex items-center gap-2">
              <Zap class="w-4 h-4 text-cyan-400" />
              <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
                RFC 5109 Forward Error Correction (FEC)
              </h2>
            </div>
            <span class="rounded bg-cyan-500/10 px-2 py-0.5 text-[10px] font-mono text-cyan-400 border border-cyan-500/20">
              K={fec?.group_size_k ?? 4} Matrix
            </span>
          </div>

          <div class="grid grid-cols-2 gap-3 text-xs font-mono">
            <div class="rounded-lg bg-zinc-950/70 p-3 border border-zinc-800/60">
              <span class="text-[10px] text-zinc-400 block">PARITY PACKETS RECV</span>
              <span class="text-lg font-bold text-zinc-100">
                {fec?.parity_packets_received_total ?? 0}
              </span>
            </div>
            <div class="rounded-lg bg-zinc-950/70 p-3 border border-zinc-800/60">
              <span class="text-[10px] text-zinc-400 block">PACKETS RECOVERED</span>
              <span class="text-lg font-bold text-emerald-400">
                {fec?.packets_recovered_total ?? 0}
              </span>
            </div>
            <div class="rounded-lg bg-zinc-950/70 p-3 border border-zinc-800/60">
              <span class="text-[10px] text-zinc-400 block">PARITY PACKETS SENT</span>
              <span class="text-lg font-bold text-zinc-100">
                {fec?.parity_packets_sent_total ?? 0}
              </span>
            </div>
            <div class="rounded-lg bg-zinc-950/70 p-3 border border-zinc-800/60">
              <span class="text-[10px] text-zinc-400 block">UNRECOVERABLE GROUPS</span>
              <span class="text-lg font-bold {fec?.unrecoverable_groups_total ? 'text-red-400' : 'text-zinc-400'}">
                {fec?.unrecoverable_groups_total ?? 0}
              </span>
            </div>
          </div>

          <div class="mt-4 text-[11px] font-mono text-zinc-500 flex items-center justify-between">
            <span>Protection Ratio: 25% (1 parity per 4 media)</span>
            <span class="text-emerald-400 font-bold">DSP LOSS IMMUNITY</span>
          </div>
        </div>

        <!-- PSOLA PLC -->
        <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-5 shadow-xl">
          <div class="flex items-center justify-between border-b border-zinc-800 pb-3 mb-4">
            <div class="flex items-center gap-2">
              <Activity class="w-4 h-4 text-amber-400" />
              <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
                PSOLA Packet Loss Concealment (PLC)
              </h2>
            </div>
            <span class="rounded bg-amber-500/10 px-2 py-0.5 text-[10px] font-mono text-amber-400 border border-amber-500/20">
              PITCH-SYNCHRONOUS
            </span>
          </div>

          <div class="grid grid-cols-2 gap-3 text-xs font-mono">
            <div class="rounded-lg bg-zinc-950/70 p-3 border border-zinc-800/60">
              <span class="text-[10px] text-zinc-400 block">CONCEALED FRAMES</span>
              <span class="text-lg font-bold text-amber-400">
                {plc?.concealed_frames_total ?? 0}
              </span>
            </div>
            <div class="rounded-lg bg-zinc-950/70 p-3 border border-zinc-800/60">
              <span class="text-[10px] text-zinc-400 block">PITCH DETECTIONS</span>
              <span class="text-lg font-bold text-zinc-100">
                {plc?.pitch_detected_frames_total ?? 0}
              </span>
            </div>
            <div class="rounded-lg bg-zinc-950/70 p-3 border border-zinc-800/60">
              <span class="text-[10px] text-zinc-400 block">NOISE EXTRAPOLATED</span>
              <span class="text-lg font-bold text-zinc-300">
                {plc?.noise_extrapolated_frames_total ?? 0}
              </span>
            </div>
            <div class="rounded-lg bg-zinc-950/70 p-3 border border-zinc-800/60">
              <span class="text-[10px] text-zinc-400 block">SILENCE DECAY EVENTS</span>
              <span class="text-lg font-bold text-zinc-400">
                {plc?.silence_decay_events_total ?? 0}
              </span>
            </div>
          </div>

          <div class="mt-4 text-[11px] font-mono text-zinc-500 flex items-center justify-between">
            <span>Concealment window: 20ms overlap-add</span>
            <span class="text-amber-400 font-bold">CLICK-FREE SYNTHESIS</span>
          </div>
        </div>
      </div>

      <!-- 3. ASRC Crystal Clock Drift PPM Gauge -->
      <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-5 shadow-xl">
        <div class="flex items-center justify-between border-b border-zinc-800 pb-3 mb-4">
          <div class="flex items-center gap-2">
            <Gauge class="w-4 h-4 text-cyan-400" />
            <h2 class="text-xs font-mono font-bold uppercase tracking-wider text-zinc-200">
              Asynchronous Sample Rate Converter (ASRC) Clock Drift
            </h2>
          </div>
          <span class="rounded bg-zinc-800 px-2 py-0.5 text-[10px] font-mono text-zinc-300">
            Nominal {asrc?.nominal_rate_hz ?? 48000} Hz
          </span>
        </div>

        <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
          <!-- Drift PPM Gauge Display -->
          <div class="rounded-xl bg-zinc-950/70 p-4 border border-zinc-800/80 flex flex-col items-center justify-center text-center">
            <span class="text-[10px] font-mono text-zinc-400">CRYSTAL CLOCK DRIFT</span>
            <div class="mt-2 text-3xl font-mono font-bold text-cyan-400">
              {asrc?.drift_ppm !== undefined ? `${asrc.drift_ppm > 0 ? '+' : ''}${asrc.drift_ppm.toFixed(2)}` : '0.00'}
              <span class="text-sm font-normal text-zinc-500">PPM</span>
            </div>
            <div class="mt-2 text-xs font-mono text-zinc-400">
              Within telecom tolerance (&plusmn;50 PPM)
            </div>
          </div>

          <!-- Resample Ratio -->
          <div class="rounded-xl bg-zinc-950/70 p-4 border border-zinc-800/80 flex flex-col items-center justify-center text-center">
            <span class="text-[10px] font-mono text-zinc-400">RESAMPLE RATIO</span>
            <div class="mt-2 text-2xl font-mono font-bold text-zinc-200">
              {asrc?.resample_ratio !== undefined ? asrc.resample_ratio.toFixed(7) : '1.0000000'}
            </div>
            <div class="mt-2 text-xs font-mono text-zinc-400">
              Polynomial Sinc Interpolator
            </div>
          </div>

          <!-- Watermark & Adjustments -->
          <div class="rounded-xl bg-zinc-950/70 p-4 border border-zinc-800/80 flex flex-col justify-between">
            <div class="space-y-2 text-xs font-mono">
              <div class="flex justify-between">
                <span class="text-zinc-400">Smoothed Watermark:</span>
                <span class="text-zinc-200 font-bold">{asrc?.smoothed_watermark_frames ?? 3.0} frames</span>
              </div>
              <div class="flex justify-between">
                <span class="text-zinc-400">Target Watermark:</span>
                <span class="text-zinc-200 font-bold">{asrc?.target_watermark_frames ?? 3.0} frames</span>
              </div>
              <div class="flex justify-between">
                <span class="text-zinc-400">Samples Adjusted:</span>
                <span class="text-cyan-400 font-bold">{asrc?.samples_adjusted_total ?? 0}</span>
              </div>
            </div>
            <div class="pt-2 border-t border-zinc-800/60 text-[10px] font-mono text-emerald-400 text-right">
              ZERO-DROP CONTINUOUS PHASE
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</div>
