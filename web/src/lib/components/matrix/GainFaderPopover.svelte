<script lang="ts">
  import { matrixStore } from '$lib/stores/matrix.svelte';
  import { deviceStore } from '$lib/stores/devices.svelte';
  import { linearToDb, dbToLinear, formatDb } from '$lib/api/types';
  import X from '@lucide/svelte/icons/x';
  import Volume2 from '@lucide/svelte/icons/volume-2';
  import VolumeX from '@lucide/svelte/icons/volume-x';
  import Zap from '@lucide/svelte/icons/zap';

  let cell = $derived(matrixStore.activeFaderCell);
  let coords = $derived(matrixStore.activeFaderCoords);

  let sourceDevice = $derived(
    matrixStore.activeFaderSourceDevice ??
    deviceStore.devices.find((d) => d.id === cell?.sourceDeviceId)
  );
  let destDevice = $derived(
    matrixStore.activeFaderDestinationDevice ??
    deviceStore.devices.find((d) => d.id === cell?.destinationDeviceId)
  );

  let sourceName = $derived(sourceDevice?.name || cell?.sourceDeviceId || 'Input Source');
  let destName = $derived(destDevice?.name || cell?.destinationDeviceId || 'Output Destination');

  // Local fader state in dB
  let currentDb = $state(0);
  let currentMuted = $state(false);
  let showSlewAnimation = $state(false);

  // Sync state whenever active cell changes
  $effect(() => {
    if (cell) {
      currentDb = cell.gainLinear > 0.0001 ? linearToDb(cell.gainLinear) : -96;
      currentMuted = cell.muted;
    }
  });

  function handleDbChange(newDb: number) {
    currentDb = newDb;
    const linear = dbToLinear(newDb);
    triggerGainUpdate(linear);
  }

  function handleSliderInput(e: Event) {
    const val = parseFloat((e.target as HTMLInputElement).value);
    currentDb = val;
    const linear = dbToLinear(val);
    triggerGainUpdate(linear);
  }

  function triggerGainUpdate(gainLinear: number) {
    if (!cell) return;
    showSlewAnimation = true;
    matrixStore.setGain(cell.routeId, gainLinear, 20.0);
    setTimeout(() => {
      showSlewAnimation = false;
    }, 150);
  }

  function toggleMute() {
    if (!cell) return;
    currentMuted = !currentMuted;
    matrixStore.setMute(cell.routeId, currentMuted);
  }

  function setUnity() {
    handleDbChange(0.0);
  }

  function setMuteZero() {
    handleDbChange(-96);
  }

  function close() {
    matrixStore.closeFaderPopover();
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') close();
  }
</script>

<svelte:window onkeydown={handleKeydown} />

{#if cell && coords}
  <!-- Overlay Backdrop to capture outside clicks -->
  <button
    type="button"
    class="fixed inset-0 z-50 bg-black/40 cursor-default border-none outline-none"
    onclick={close}
    aria-label="Close Popover"
  ></button>

  <!-- Floating Fader Card -->
  <div
    class="fixed z-50 w-72 rounded-xl border border-zinc-700/80 bg-zinc-950 p-4 shadow-2xl shadow-cyan-950/40 text-zinc-100 animate-in fade-in zoom-in-95"
    style="top: {Math.max(10, Math.min(window.innerHeight - 420, coords.y - 120))}px; left: {Math.max(10, Math.min(window.innerWidth - 310, coords.x + 24))}px;"
  >
    <!-- Header -->
    <div class="flex items-start justify-between border-b border-zinc-800 pb-2.5 mb-3">
      <div class="flex flex-col min-w-0 pr-2">
        <span class="text-[10px] font-mono font-semibold tracking-wider text-cyan-400">
          DEVICE ROUTE GAIN
        </span>
        <div class="text-xs font-mono text-zinc-200 font-medium truncate mt-0.5" title="{sourceName} &rarr; {destName}">
          <span class="text-cyan-300">{sourceName}</span>
          <span class="text-zinc-500 mx-1">&rarr;</span>
          <span class="text-zinc-100">{destName}</span>
        </div>
        <div class="flex items-center gap-2 mt-1 text-[10px] font-mono text-zinc-400">
          <span>In: {sourceDevice?.max_input ?? 2} Ch ({sourceDevice?.is_virtual ? 'VIRT' : 'HW'})</span>
          <span>&bull;</span>
          <span>Out: {destDevice?.max_output ?? 2} Ch ({destDevice?.is_virtual ? 'VIRT' : 'HW'})</span>
        </div>
      </div>
      <button
        onclick={close}
        class="rounded p-1 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-200 transition-colors shrink-0"
        aria-label="Close"
      >
        <X class="w-3.5 h-3.5" />
      </button>
    </div>

    <!-- Slew Anti-Zipper Indicator -->
    <div class="mb-3 flex items-center justify-between rounded-lg border border-zinc-800 bg-zinc-900/80 px-2.5 py-1.5 text-[11px] font-mono">
      <div class="flex items-center gap-1.5">
        <Zap class="w-3 h-3 {showSlewAnimation ? 'text-amber-400 animate-bounce' : 'text-zinc-500'}" />
        <span class="text-zinc-400">DSP Slew:</span>
        <span class="{showSlewAnimation ? 'text-amber-300 font-bold' : 'text-zinc-300'}">20 ms</span>
      </div>
      <span class="text-[10px] text-zinc-400">anti-zipper</span>
    </div>

    <!-- Vertical Fader & Gain Meter Display -->
    <div class="flex gap-4 items-center justify-center py-2">
      <!-- Vertical Gain Slider -->
      <div class="relative flex flex-col items-center h-48 w-12 justify-center">
        <!-- Scale ticks -->
        <div class="absolute left-0 flex flex-col justify-between h-40 text-[9px] font-mono text-zinc-500 select-none pointer-events-none">
          <span>+6</span>
          <span class="text-cyan-400 font-bold">0</span>
          <span>-6</span>
          <span>-18</span>
          <span>-36</span>
          <span>-&infin;</span>
        </div>

        <input
          type="range"
          min="-96"
          max="6"
          step="0.5"
          value={currentDb}
          oninput={handleSliderInput}
          class="fader-slider -rotate-90 w-40 cursor-pointer accent-cyan-400 bg-zinc-800"
          aria-label="Gain Slider"
        />
      </div>

      <!-- Live Gain readout & Preset Quick Buttons -->
      <div class="flex flex-col gap-2.5 flex-1">
        <!-- Large readout -->
        <div class="rounded-lg border border-zinc-800 bg-zinc-900 p-2.5 text-center">
          <div class="text-[10px] font-mono text-zinc-500">CURRENT GAIN</div>
          <div class="text-xl font-mono font-bold {currentDb >= 0 ? 'text-cyan-400' : currentDb <= -90 ? 'text-zinc-500' : 'text-zinc-200'}">
            {formatDb(currentDb)}
          </div>
          <div class="text-[10px] font-mono text-zinc-400">
            x{dbToLinear(currentDb).toFixed(3)} lin
          </div>
        </div>

        <!-- Unity Gain Button -->
        <button
          type="button"
          onclick={setUnity}
          class="w-full rounded border border-cyan-500/30 bg-cyan-500/10 py-1 text-xs font-mono font-semibold text-cyan-300 hover:bg-cyan-500/20 transition-all"
        >
          0.0 dB (Unity)
        </button>

        <!-- -inf dB Button -->
        <button
          type="button"
          onclick={setMuteZero}
          class="w-full rounded border border-zinc-800 bg-zinc-900 py-1 text-xs font-mono text-zinc-400 hover:bg-zinc-800 hover:text-zinc-200 transition-all"
        >
          -&infin; dB (Cut)
        </button>
      </div>
    </div>

    <!-- Mute Switch -->
    <div class="mt-2 pt-2.5 border-t border-zinc-800 flex items-center justify-between">
      <span class="text-xs font-medium text-zinc-300">Route Mute</span>
      <button
        type="button"
        onclick={toggleMute}
        class="inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-semibold transition-all {currentMuted
          ? 'bg-red-500 text-white shadow-sm shadow-red-500/40'
          : 'bg-zinc-800 text-zinc-300 hover:bg-zinc-700'}"
      >
        {#if currentMuted}
          <VolumeX class="w-3.5 h-3.5" />
          <span>MUTED</span>
        {:else}
          <Volume2 class="w-3.5 h-3.5 text-emerald-400" />
          <span>UNMUTED</span>
        {/if}
      </button>
    </div>
  </div>
{/if}

<style>
  .fader-slider {
    -webkit-appearance: none;
    appearance: none;
    height: 8px;
    border-radius: 4px;
    outline: none;
  }
  .fader-slider::-webkit-slider-thumb {
    -webkit-appearance: none;
    appearance: none;
    width: 22px;
    height: 26px;
    border-radius: 4px;
    background: #06b6d4;
    border: 2px solid #ffffff;
    cursor: pointer;
    box-shadow: 0 0 10px rgba(6, 182, 212, 0.6);
  }
  .fader-slider::-moz-range-thumb {
    width: 22px;
    height: 26px;
    border-radius: 4px;
    background: #06b6d4;
    border: 2px solid #ffffff;
    cursor: pointer;
    box-shadow: 0 0 10px rgba(6, 182, 212, 0.6);
  }
</style>
