<script lang="ts">
  import type { Snippet } from 'svelte';
  import { formatScore, severityBadgeClass, severityDotClass, severityLabel } from '$lib/utils/artifacts';

  interface Props {
    /**
     * ADR 0034 4-tier ArtifactSeverity (NONE/MINOR/MAJOR/CRITICAL).
     * NOTE: this is NOT the 5-tier NodeHealthGrade — separate vocabulary,
     * never cross-mapped. Unknown values render neutral (zinc), never clean.
     */
    severity: string | null | undefined;
    /** Optional 0–100 audibility score rendered after the label. */
    score?: number | null;
    /** Optional popover lines shown on hover (e.g. from breakdownLines). */
    lines?: string[];
    /** Optional a11y/native title override. */
    title?: string;
    /** Optional rich tooltip content; used when `lines` is empty. */
    children?: Snippet;
  }

  let { severity, score = null, lines = [], title = '', children }: Props = $props();

  const label = $derived(severityLabel(severity));
  const showScore = $derived(score !== null && score !== undefined && Number.isFinite(score));

  const nativeTitle = $derived(
    title ||
      `Audio artifact health: ${label}${showScore ? ` (score ${formatScore(score!)})` : ''}`
  );
</script>

<span
  class="group relative inline-flex items-center shrink-0"
  role="status"
  aria-label={nativeTitle}
>
  <span
    class="inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[9px] font-mono font-bold uppercase tracking-wider transition-colors {severityBadgeClass(severity)}
      {severity === 'CRITICAL' ? 'animate-pulse' : ''}"
    title={nativeTitle}
  >
    <span class="h-1.5 w-1.5 rounded-full shrink-0 {severityDotClass(severity)}" aria-hidden="true"></span>
    <span>{label}</span>
    {#if showScore}
      <span class="font-semibold opacity-70">{formatScore(score!)}</span>
    {/if}
  </span>

  {#if lines.length > 0 || children}
    <!-- Hover popover: per-route windowed breakdown + distinct shared-device line (ADR 0034 §6) -->
    <span
      class="pointer-events-none invisible absolute left-0 top-full z-50 mt-1.5 w-max max-w-80 rounded-lg border border-zinc-700 bg-zinc-950/95 p-2.5 text-left text-[10px] font-mono normal-case tracking-normal text-zinc-200 shadow-2xl backdrop-blur-md transition-opacity duration-100 group-hover:visible group-hover:opacity-100"
    >
      {#if lines.length > 0}
        <span class="block max-h-64 overflow-y-auto">
          {#each lines as line (line)}
            <span class="block whitespace-pre-wrap leading-relaxed break-words">{line}</span>
          {/each}
        </span>
      {:else if children}
        {@render children()}
      {/if}
    </span>
  {/if}
</span>
