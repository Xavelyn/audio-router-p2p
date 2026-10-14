/**
 * ADR 0034 — Per-Connection Audio Artifact Detection and Health.
 *
 * Pure helper functions for rendering the 4-tier `ArtifactSeverity`
 * vocabulary (NONE / MINOR / MAJOR / CRITICAL). NO DOM, NO Svelte runes:
 * every function is unit-testable in isolation (web/src/tests/artifacts.test.ts).
 *
 * Attribution discipline (ADR 0034 §1.C): route scores use ONLY route-scoped
 * counters; hardware XRuns are shared per physical device and are surfaced
 * separately (device pill = worst of device + touching routes; tooltips carry
 * a distinct "shared device" line). The 5-tier `NodeHealthGrade`
 * (EXCELLENT/GOOD/FAIR/POOR/CRITICAL) is a DIFFERENT vocabulary (link quality)
 * and is deliberately never cross-mapped here: any value outside the 4-tier
 * set is treated as *unknown*, which renders neutral (zinc) and ranks as
 * worst for fail-safe ordering.
 */
import type {
  ArtifactSeverity,
  DeviceHealth,
  RouteHealth,
  RouteDeviceImpact,
} from '$lib/api/types';

/**
 * Existing staleness convention used by the flow-canvas meter binding
 * (ADR 0026, 1500 ms). Artifact telemetry fetched on the 2 s REST polling
 * loop is considered stale past this threshold, at which point surfaces must
 * render nothing rather than a possibly-stale (or fake-clean) indicator.
 */
export const TELEMETRY_STALE_MS = 1500;

/** Ordinal ordering of the 4-tier ArtifactSeverity vocabulary. */
export const SEVERITY_ORDER: Readonly<Record<ArtifactSeverity, number>> = {
  NONE: 0,
  MINOR: 1,
  MAJOR: 2,
  CRITICAL: 3,
};

/** Absent (null/undefined/empty) is "no severity", not a rank. */
export const ABSENT_SEVERITY_RANK = -1;

/** Unknown strings rank ABOVE CRITICAL: an unrecognized tier must never look healthy. */
const UNKNOWN_SEVERITY_RANK = SEVERITY_ORDER.CRITICAL + 1;

/**
 * Numeric rank of a severity value.
 *  - absent (null/undefined/'') -> -1
 *  - known 4-tier value -> 0..3
 *  - unknown string (incl. NodeHealthGrade values) -> 4 (fail-safe worst)
 */
export function severityRank(severity: string | null | undefined): number {
  if (severity === null || severity === undefined || severity === '') {
    return ABSENT_SEVERITY_RANK;
  }
  const rank = SEVERITY_ORDER[severity as ArtifactSeverity];
  return rank === undefined ? UNKNOWN_SEVERITY_RANK : rank;
}

/**
 * The worse of two severities. Returns null only when both are absent.
 * Ties return `a` (deterministic). Unknown severities outrank CRITICAL.
 */
export function worstSeverity(
  a: string | null | undefined,
  b: string | null | undefined
): string | null {
  const ra = severityRank(a);
  const rb = severityRank(b);
  if (ra < 0 && rb < 0) return null;
  if (ra < 0) return b ?? null;
  if (rb < 0) return a ?? null;
  // A positive rank guarantees presence; the `?? null` is for the type system.
  return ra >= rb ? (a ?? b ?? null) : (b ?? null);
}

/**
 * The severity a flow-canvas device pill must show (ADR 0034 §6): the WORST of
 * the device's own severity (URN-keyed device-artifacts) and the severities of
 * ALL routes touching the device. Returns null when no data exists at all
 * (aggregator unwired) — callers must render nothing, never a fake-clean pill.
 */
export function devicePillSeverity(
  deviceSeverity: string | null | undefined,
  routeSeverities: Array<string | null | undefined> | null | undefined
): string | null {
  let worst: string | null = null;
  worst = worstSeverity(worst, deviceSeverity);
  for (const s of routeSeverities ?? []) {
    worst = worstSeverity(worst, s);
  }
  return worst;
}

/** Compact pill label (max 4 letters). */
export function severityLabel(severity: string | null | undefined): string {
  switch (severity) {
    case 'NONE':
      return 'OK';
    case 'MINOR':
      return 'MIN';
    case 'MAJOR':
      return 'MAJ';
    case 'CRITICAL':
      return 'CRIT';
    default:
      return '???';
  }
}

/**
 * Tailwind classes for the pill container (bg/text/border) matching the
 * app's existing badge idiom. Green/amber/orange/red for the 4 tiers;
 * neutral zinc for unknown values (never a fake-clean green).
 */
export function severityBadgeClass(severity: string | null | undefined): string {
  switch (severity) {
    case 'NONE':
      return 'bg-emerald-500/15 text-emerald-300 border-emerald-500/40';
    case 'MINOR':
      return 'bg-amber-500/15 text-amber-300 border-amber-500/40';
    case 'MAJOR':
      return 'bg-orange-500/15 text-orange-300 border-orange-500/40';
    case 'CRITICAL':
      return 'bg-red-500/15 text-red-300 border-red-500/40';
    default:
      return 'bg-zinc-800 text-zinc-400 border-zinc-700';
  }
}

/** Tailwind class for the severity dot. */
export function severityDotClass(severity: string | null | undefined): string {
  switch (severity) {
    case 'NONE':
      return 'bg-emerald-400';
    case 'MINOR':
      return 'bg-amber-400';
    case 'MAJOR':
      return 'bg-orange-400';
    case 'CRITICAL':
      return 'bg-red-400 shadow-[0_0_6px_rgba(248,113,113,0.7)]';
    default:
      return 'bg-zinc-500';
  }
}

/**
 * Format a per-second event rate for compact chips/tooltips.
 * 0 -> "0/s", <1 -> 2 decimals, <10 -> 1 decimal, else rounded integer.
 */
export function formatRate(perSecond: number): string {
  if (!Number.isFinite(perSecond) || perSecond <= 0) return '0/s';
  if (perSecond < 1) return `${perSecond.toFixed(2)}/s`;
  if (perSecond < 10) return `${perSecond.toFixed(1)}/s`;
  return `${Math.round(perSecond)}/s`;
}

/** Compact 0–100 score formatting (integers stay integers). */
export function formatScore(score: number): string {
  if (!Number.isFinite(score)) return '—';
  return Number.isInteger(score) ? String(score) : score.toFixed(1);
}

/**
 * Freshness gate for artifact telemetry (existing 1500 ms staleness
 * convention). `lastUpdated` is the epoch-ms of the last successful
 * telemetry tick; 0 means "never fetched" (no data).
 */
export function isTelemetryFresh(lastUpdated: number, now: number, staleMs: number = TELEMETRY_STALE_MS): boolean {
  return lastUpdated > 0 && now - lastUpdated <= staleMs;
}

/**
 * Identifier tokens of a flow-canvas device node, mirroring the meter
 * stream-binding predicate (URN / checksum / tail / legacy_id, ADR 0026).
 */
export interface DeviceIdentity {
  urn?: string | null;
  id?: string | null;
  legacyId?: string | null;
  checksum?: string | null;
  tail?: string | null;
}

/**
 * Does a route touch this device? (ADR 0034 §4/§6)
 * Primary: the route's `devices.input.urn` / `devices.output.urn` equals the
 * node's device URN (exact binding, no tail matching).
 * Fallback: room route IDs embed the endpoint identifiers
 * (`room-<roomId>-<inDev>-<outDev>`), so a substring match on the same
 * identifier tokens as the meter binding also binds.
 */
export function routeTouchesDevice(
  routeId: string,
  routeDevices: RouteDeviceImpact | null | undefined,
  device: DeviceIdentity
): boolean {
  const exactCandidates = [device.urn, device.id, device.legacyId].filter(
    (v): v is string => typeof v === 'string' && v.length > 0
  );
  const inputUrn = routeDevices?.input?.urn;
  const outputUrn = routeDevices?.output?.urn;
  if (inputUrn && exactCandidates.includes(inputUrn)) return true;
  if (outputUrn && exactCandidates.includes(outputUrn)) return true;

  const tokens = [device.id, device.urn, device.checksum, device.tail, device.legacyId].filter(
    (v): v is string => typeof v === 'string' && v.length > 0
  );
  return tokens.some((token) => routeId.includes(token));
}

/** The route peer with the highest underruns+overruns+late_packets sum. */
export interface WorstPeer {
  addr: string;
  underruns: number;
  overruns: number;
  late_packets: number;
  total: number;
}

/**
 * Deterministic "worst peer" for the per-route table: highest
 * underruns + overruns + late_packets sum, ties broken by lexicographically
 * smallest address. Returns null when the route carries no peers.
 */
export function worstPeer(health: RouteHealth | null | undefined): WorstPeer | null {
  const peers = health?.peers;
  if (!peers) return null;
  let best: WorstPeer | null = null;
  for (const [addr, p] of Object.entries(peers)) {
    if (!p) continue;
    const underruns = p.underruns || 0;
    const overruns = p.overruns || 0;
    const late_packets = p.late_packets || 0;
    const total = underruns + overruns + late_packets;
    if (!best || total > best.total || (total === best.total && addr < best.addr)) {
      best = { addr, underruns, overruns, late_packets, total };
    }
  }
  return best;
}

/** One route row of the hover popover / diagnostics breakdown. */
export interface RouteBreakdownEntry {
  routeId: string;
  health: RouteHealth | null | undefined;
}

/** The shared device row of the hover popover / diagnostics breakdown. */
export interface DeviceBreakdownEntry {
  urn: string;
  health: DeviceHealth | null | undefined;
}

/**
 * Hover-popover breakdown lines (ADR 0034 §6): one windowed line per touching
 * route (route ID, score/severity, underruns/overruns/ring-drops with e/s
 * rates, sorted by route ID for determinism) followed by a DISTINCT
 * "shared device" line (device URN, per-direction XRun window/total, severity)
 * so operators never mistake a device XRun for a route defect.
 */
export function breakdownLines(
  device: DeviceBreakdownEntry | null | undefined,
  routes: Array<RouteBreakdownEntry> | null | undefined
): string[] {
  const lines: string[] = [];

  const sorted = (routes ?? [])
    .filter((r): r is { routeId: string; health: RouteHealth } => !!r && !!r.health)
    .sort((a, b) => a.routeId.localeCompare(b.routeId));

  for (const r of sorted) {
    const h = r.health;
    const rates = h.rates ?? {
      underrun_per_s: 0,
      overrun_per_s: 0,
      xrun_per_s: 0,
      ring_drop_per_s: 0,
    };
    lines.push(
      `${r.routeId} · ${formatScore(h.score)} ${h.severity} · ` +
        `U ${h.underruns} (${formatRate(rates.underrun_per_s)}) · ` +
        `O ${h.overruns} (${formatRate(rates.overrun_per_s)}) · ` +
        `RD ${h.ring_drops} (${formatRate(rates.ring_drop_per_s)})`
    );
  }

  if (device && device.urn) {
    const h = device.health;
    let line = `SHARED DEVICE ${device.urn} · ${h ? `${h.severity} ${formatScore(h.score)}` : 'no data'}`;
    if (h?.ghost) line += ' · GHOST';
    if (h?.input) line += ` · in XRun ${h.input.xrun_window}/${h.input.xrun_total}`;
    if (h?.output) line += ` · out XRun ${h.output.xrun_window}/${h.output.xrun_total}`;
    lines.push(line);
  }

  return lines;
}
