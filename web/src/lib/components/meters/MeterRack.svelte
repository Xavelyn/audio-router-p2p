<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { meterStore, type ChannelState } from '$lib/stores/meters.svelte';
  import Activity from '@lucide/svelte/icons/activity';
  import Radio from '@lucide/svelte/icons/radio';
  import Zap from '@lucide/svelte/icons/zap';
  import Volume2 from '@lucide/svelte/icons/volume-2';

  let canvasElements: Record<number, HTMLCanvasElement> = {};
  let animId: number | null = null;

  onMount(() => {
    meterStore.init();
    startCanvasRenderLoop();
  });

  onDestroy(() => {
    if (animId) cancelAnimationFrame(animId);
  });

  function dbToCanvasRatio(db: number): number {
    // Map -60 dBFS to 0 dBFS to 0.0 .. 1.0 (clamped)
    if (db <= -60) return 0;
    if (db >= 0) return 1;
    return (db + 60) / 60;
  }

  function startCanvasRenderLoop() {
    const render = () => {
      const channels = meterStore.activeChannelMeters;

      for (const ch of channels) {
        const canvas = canvasElements[ch.channel];
        if (!canvas) continue;
        const ctx = canvas.getContext('2d');
        if (!ctx) continue;

        const w = canvas.width;
        const h = canvas.height;

        // Background
        ctx.fillStyle = '#09090b';
        ctx.fillRect(0, 0, w, h);

        // Grid / tick marks background
        const ticks = [0, -3, -6, -12, -18, -24, -36, -48];
        ctx.fillStyle = '#1e1e24';
        for (const t of ticks) {
          const y = h - dbToCanvasRatio(t) * h;
          ctx.fillRect(0, y, w, 1);
        }

        // Draw RMS Bar
        const rmsRatio = dbToCanvasRatio(ch.rmsDb);
        const barHeight = rmsRatio * h;
        const barY = h - barHeight;

        // Gradient for RMS bar (green -> amber -> red)
        const grad = ctx.createLinearGradient(0, h, 0, 0);
        grad.addColorStop(0, '#10b981'); // Emerald green
        grad.addColorStop(0.7, '#22c55e'); // Signal green
        grad.addColorStop(0.85, '#eab308'); // Amber warning
        grad.addColorStop(0.98, '#ef4444'); // Clip red

        ctx.fillStyle = grad;
        ctx.fillRect(4, barY, w - 8, barHeight);

        // Peak Level marker (solid bar line)
        const peakRatio = dbToCanvasRatio(ch.peakDb);
        const peakY = h - peakRatio * h;
        ctx.fillStyle = '#ffffff';
        ctx.fillRect(2, peakY, w - 4, 2);

        // Peak Hold Needle (cyan neon line)
        const holdRatio = dbToCanvasRatio(ch.peakHoldDb);
        const holdY = h - holdRatio * h;
        ctx.fillStyle = '#06b6d4';
        ctx.shadowColor = '#06b6d4';
        ctx.shadowBlur = 6;
        ctx.fillRect(2, holdY - 1, w - 4, 3);
        ctx.shadowBlur = 0; // Reset shadow
      }

      animId = requestAnimationFrame(render);
    };

    animId = requestAnimationFrame(render);
  }

  function getDtxBadge(state: string) {
    switch (state) {
      case 'Active':
        return { label: 'VOICE ACTIVE', color: 'bg-emerald-500/20 text-emerald-400 border-emerald-500/30' };
      case 'Hangover':
        return { label: 'HANGOVER', color: 'bg-amber-500/20 text-amber-400 border-amber-500/30' };
      case 'Suppressed':
        return { label: 'DTX SILENCE', color: 'bg-zinc-800 text-zinc-400 border-zinc-700' };
      default:
        return { label: 'VOICE ACTIVE', color: 'bg-emerald-500/20 text-emerald-400 border-emerald-500/30' };
    }
  }
</script>

<div class="flex flex-col h-full bg-studio-dark select-none overflow-hidden">
  <!-- Top Meter Toolbar -->
  <div class="flex items-center justify-between border-b border-zinc-800 bg-zinc-950/70 px-4 py-2.5">
    <div class="flex items-center gap-3">
      <div class="flex items-center gap-2">
        <Activity class="w-4 h-4 text-cyan-400" />
        <h1 class="text-sm font-semibold tracking-wide text-zinc-100 uppercase">
          Precision Studio Meter Rack
        </h1>
      </div>
      <span class="rounded bg-zinc-800 px-2 py-0.5 text-xs font-mono text-zinc-300">
        60 FPS True-Peak & RMS
      </span>
    </div>

    <!-- Actions & Controls -->
    <div class="flex items-center gap-3">
      <!-- Test Tone / Generator Toggle -->
      <button
        onclick={() => meterStore.toggleSimulation()}
        class="inline-flex items-center gap-1.5 rounded-lg border px-3 py-1.5 text-xs font-mono font-medium transition-all {meterStore.useSimulation
          ? 'border-cyan-500 bg-cyan-500/20 text-cyan-300 shadow-sm shadow-cyan-500/30'
          : 'border-zinc-800 bg-zinc-900 text-zinc-400 hover:text-zinc-200'}"
      >
        <Zap class="w-3.5 h-3.5 {meterStore.useSimulation ? 'text-cyan-400 animate-pulse' : ''}" />
        <span>{meterStore.useSimulation ? 'Simulation ON' : 'Test Signal Gen'}</span>
      </button>

      <!-- Connection state pill -->
      <div class="flex items-center gap-1.5 rounded-lg border border-zinc-800 bg-zinc-900/80 px-2.5 py-1 text-xs font-mono">
        <span class="h-2 w-2 rounded-full {meterStore.isConnected ? 'bg-emerald-500 shadow-[0_0_8px_rgba(16,185,129,0.8)]' : 'bg-amber-500'}"></span>
        <span class="text-zinc-300">{meterStore.isConnected ? 'TELEMETRY LIVE' : 'BUFFERING'}</span>
      </div>
    </div>
  </div>

  <!-- Main Meter Bridges Container -->
  <div class="flex-1 overflow-y-auto p-6 flex flex-col items-center justify-center bg-zinc-950">
    <div class="flex flex-col gap-6 w-full max-w-5xl">
      <!-- Streams Loop or Multi-channel Rack -->
      {#if Object.keys(meterStore.streams).length === 0 && !meterStore.useSimulation}
        <div class="flex flex-col items-center justify-center p-12 rounded-xl border border-dashed border-zinc-800 text-center">
          <Activity class="w-8 h-8 text-zinc-600 mb-2" />
          <h3 class="text-sm font-semibold text-zinc-300">No active audio stream telemetry frames</h3>
          <p class="text-xs text-zinc-500 max-w-sm mt-1">
            Audio streams will automatically appear here when routes are active, or you can enable the test signal generator.
          </p>
          <button
            onclick={() => meterStore.toggleSimulation(true)}
            class="mt-4 inline-flex items-center gap-1.5 rounded-lg bg-cyan-500 px-3 py-1.5 text-xs font-semibold text-zinc-950 shadow-sm shadow-cyan-500/30 hover:bg-cyan-400 transition-colors"
          >
            <Zap class="w-3.5 h-3.5" />
            <span>Enable Test Signal Generator</span>
          </button>
        </div>
      {:else}
        {#each Object.values(meterStore.streams) as stream}
          {@const dtxBadge = getDtxBadge(stream.dtxState)}
          <div class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-5 shadow-xl">
            <!-- Stream Header -->
            <div class="flex items-center justify-between border-b border-zinc-800/80 pb-3 mb-4">
              <div class="flex items-center gap-2.5">
                <Radio class="w-4 h-4 text-cyan-400" />
                <span class="font-mono text-xs font-bold text-zinc-200 uppercase">
                  Stream ID: {stream.streamId}
                </span>
              </div>
              <div class="flex items-center gap-3">
                <span class="border rounded-md px-2 py-0.5 text-[10px] font-mono font-bold {dtxBadge.color}">
                  {dtxBadge.label}
                </span>
                <span class="text-[10px] font-mono text-zinc-400">
                  {stream.channels.length} CHANNELS
                </span>
              </div>
            </div>

            <!-- Meter Channel Strips Horizontal Rack -->
            <div class="flex items-center justify-center gap-6 py-2 overflow-x-auto">
              <!-- Scale Column -->
              <div class="flex flex-col justify-between h-64 text-[10px] font-mono text-zinc-400 py-1 pr-1 select-none text-right">
                <span class="text-red-400 font-bold">CLIP (0)</span>
                <span class="text-amber-400">-3 dB</span>
                <span class="text-amber-400">-6 dB</span>
                <span class="text-emerald-400">-12 dB</span>
                <span class="text-emerald-400">-18 dB</span>
                <span class="text-zinc-400">-24 dB</span>
                <span class="text-zinc-400">-36 dB</span>
                <span class="text-zinc-400">-48 dB</span>
                <span class="text-zinc-400">-60 dB</span>
              </div>

              <!-- Channel Meters -->
              {#each stream.channels as ch}
                <div class="flex flex-col items-center gap-2">
                  <!-- Clip LED Lamp -->
                  <div
                    class="h-3 w-8 rounded-sm border transition-all flex items-center justify-center {ch.clipped
                      ? 'bg-red-500 border-red-300 shadow-[0_0_12px_rgba(239,68,68,1)] animate-pulse'
                      : 'bg-zinc-900 border-zinc-800'}"
                    title="Digital Ceiling Clip (>-0.1 dBFS)"
                  >
                    <span class="text-[7px] font-mono font-bold {ch.clipped ? 'text-white' : 'text-zinc-600'}">
                      CLIP
                    </span>
                  </div>

                  <!-- Canvas Vertical VU Tube -->
                  <div class="relative rounded-md border border-zinc-800 bg-zinc-950 p-1 shadow-inner">
                    <canvas
                      bind:this={canvasElements[ch.channel]}
                      width="36"
                      height="240"
                      class="rounded bg-zinc-950"
                    ></canvas>
                  </div>

                  <!-- Peak Readout Pill -->
                  <div class="rounded border border-zinc-800 bg-zinc-950 px-1.5 py-0.5 text-[10px] font-mono font-bold {ch.peakDb > -1 ? 'text-red-400' : ch.peakDb > -6 ? 'text-amber-400' : 'text-emerald-400'}">
                    {ch.peakDb <= -60 ? '-inf' : `${ch.peakDb.toFixed(1)}`}
                  </div>

                  <!-- Channel Label -->
                  <span class="rounded bg-zinc-800/90 px-2 py-0.5 text-xs font-mono font-bold text-cyan-400 border border-zinc-700/50">
                    CH {ch.channel + 1}
                  </span>
                </div>
              {/each}
            </div>
          </div>
        {/each}
      {/if}
    </div>
  </div>
</div>
