import { describe, it, expect } from 'vitest';
import {
  SEVERITY_ORDER,
  ABSENT_SEVERITY_RANK,
  TELEMETRY_STALE_MS,
  severityRank,
  worstSeverity,
  devicePillSeverity,
  severityLabel,
  severityBadgeClass,
  severityDotClass,
  formatRate,
  formatScore,
  isTelemetryFresh,
  routeTouchesDevice,
  worstPeer,
  breakdownLines,
  type DeviceIdentity,
} from '$lib/utils/artifacts';
import type { DeviceHealth, RouteHealth } from '$lib/api/types';

// Fixtures
function mkHealth(over: Partial<RouteHealth> = {}): RouteHealth {
  return {
    score: 100,
    severity: 'NONE',
    window_s: 10,
    underruns: 0,
    overruns: 0,
    ring_drops: 0,
    plc_concealed: 0,
    fec_unrecoverable: 0,
    packets_lost: 0,
    fec_recovered: 0,
    playout_gaps: 0,
    rates: { underrun_per_s: 0, overrun_per_s: 0, xrun_per_s: 0, ring_drop_per_s: 0 },
    ghost: false,
    peers: {},
    ...over,
  };
}

function mkDevice(over: Partial<DeviceHealth> = {}): DeviceHealth {
  return {
    urn: 'alsa://hw:0,0',
    ghost: false,
    score: 100,
    severity: 'NONE',
    ...over,
  };
}

const dev = (over: Partial<DeviceIdentity> = {}): DeviceIdentity => ({
  urn: null,
  id: null,
  legacyId: null,
  checksum: null,
  tail: null,
  ...over,
});

describe('ADR-0034: Artifact severity ordering and worst-of aggregation', () => {
  it('orders the 4-tier ArtifactSeverity vocabulary NONE < MINOR < MAJOR < CRITICAL', () => {
    expect(SEVERITY_ORDER.NONE).toBe(0);
    expect(SEVERITY_ORDER.MINOR).toBe(1);
    expect(SEVERITY_ORDER.MAJOR).toBe(2);
    expect(SEVERITY_ORDER.CRITICAL).toBe(3);
  });

  it('severityRank maps known tiers, absent -> -1, unknown -> above CRITICAL (fail-safe)', () => {
    expect(severityRank('NONE')).toBe(0);
    expect(severityRank('MINOR')).toBe(1);
    expect(severityRank('MAJOR')).toBe(2);
    expect(severityRank('CRITICAL')).toBe(3);
    expect(severityRank(null)).toBe(ABSENT_SEVERITY_RANK);
    expect(severityRank(undefined)).toBe(ABSENT_SEVERITY_RANK);
    expect(severityRank('')).toBe(ABSENT_SEVERITY_RANK);
    // Unknown values (incl. the OTHER vocabulary) rank as worst, never healthy.
    expect(severityRank('EXCELLENT')).toBeGreaterThanOrEqual(SEVERITY_ORDER.CRITICAL + 1);
    expect(severityRank('GOOD')).toBeGreaterThanOrEqual(SEVERITY_ORDER.CRITICAL + 1);
    expect(severityRank('totally-unknown')).toBeGreaterThanOrEqual(SEVERITY_ORDER.CRITICAL + 1);
  });

  it('worstSeverity returns null only when both inputs are absent', () => {
    expect(worstSeverity(null, null)).toBeNull();
    expect(worstSeverity(undefined, undefined)).toBeNull();
    expect(worstSeverity(null, '')).toBeNull();
    expect(worstSeverity('NONE', null)).toBe('NONE');
    expect(worstSeverity(null, 'MINOR')).toBe('MINOR');
  });

  it('worstSeverity picks the higher tier regardless of argument order', () => {
    expect(worstSeverity('MAJOR', 'CRITICAL')).toBe('CRITICAL');
    expect(worstSeverity('CRITICAL', 'MAJOR')).toBe('CRITICAL');
    expect(worstSeverity('NONE', 'MINOR')).toBe('MINOR');
    expect(worstSeverity('MINOR', 'NONE')).toBe('MINOR');
  });

  it('worstSeverity tie-breaking is deterministic (first argument wins)', () => {
    expect(worstSeverity('MINOR', 'MINOR')).toBe('MINOR');
    expect(worstSeverity('MAJOR', 'MAJOR')).toBe('MAJOR');
    // Two distinct unknown strings tie at the fail-safe rank: first wins.
    expect(worstSeverity('alpha-unknown', 'beta-unknown')).toBe('alpha-unknown');
  });

  it('worstSeverity treats unknown severities as worse than CRITICAL', () => {
    expect(worstSeverity('EXCELLENT', 'NONE')).toBe('EXCELLENT');
    expect(worstSeverity('CRITICAL', 'something-unknown')).toBe('something-unknown');
  });

  it('devicePillSeverity is the worst of device severity and ALL touching route severities', () => {
    // No data at all (aggregator unwired) -> null (render nothing, never fake-clean)
    expect(devicePillSeverity(null, [])).toBeNull();
    expect(devicePillSeverity(null, undefined)).toBeNull();
    expect(devicePillSeverity(undefined, [])).toBeNull();

    // Device-only data
    expect(devicePillSeverity('NONE', [])).toBe('NONE');
    expect(devicePillSeverity('CRITICAL', [])).toBe('CRITICAL');

    // Route-only data (device absent from URN-keyed endpoint)
    expect(devicePillSeverity(null, ['MAJOR'])).toBe('MAJOR');
    expect(devicePillSeverity(undefined, ['MINOR', null, 'MAJOR'])).toBe('MAJOR');

    // Device worse than routes: device XRun visible even with clean route counters
    expect(devicePillSeverity('MAJOR', ['NONE', 'NONE'])).toBe('MAJOR');

    // Route worse than device: shared device keeps the pill honest the other way
    expect(devicePillSeverity('NONE', ['NONE', 'CRITICAL'])).toBe('CRITICAL');

    // Unknown route severity is fail-safe worst
    expect(devicePillSeverity('NONE', ['WEIRD-TIER'])).toBe('WEIRD-TIER');
  });
});

describe('ADR-0034: badge styling is its own vocabulary (no NodeHealthGrade cross-mapping)', () => {
  it('severityLabel renders the compact <=4-letter labels', () => {
    expect(severityLabel('NONE')).toBe('OK');
    expect(severityLabel('MINOR')).toBe('MIN');
    expect(severityLabel('MAJOR')).toBe('MAJ');
    expect(severityLabel('CRITICAL')).toBe('CRIT');
    expect(severityLabel(null)).toBe('???');
    expect(severityLabel('unknown')).toBe('???');
  });

  it('severityBadgeClass maps green/amber/orange/red for the 4 tiers', () => {
    expect(severityBadgeClass('NONE')).toContain('emerald');
    expect(severityBadgeClass('MINOR')).toContain('amber');
    expect(severityBadgeClass('MAJOR')).toContain('orange');
    expect(severityBadgeClass('CRITICAL')).toContain('red');
  });

  it('severityBadgeClass treats unknown values (incl. NodeHealthGrade) as neutral, NEVER clean green', () => {
    for (const grade of ['EXCELLENT', 'GOOD', 'FAIR', 'POOR', null, '']) {
      const cls = severityBadgeClass(grade);
      expect(cls).toContain('zinc');
      expect(cls).not.toContain('emerald');
    }
  });

  it('severityDotClass matches the tier colors and is neutral for unknown', () => {
    expect(severityDotClass('NONE')).toContain('emerald');
    expect(severityDotClass('MINOR')).toContain('amber');
    expect(severityDotClass('MAJOR')).toContain('orange');
    expect(severityDotClass('CRITICAL')).toContain('red');
    expect(severityDotClass('EXCELLENT')).toContain('zinc');
    expect(severityDotClass(null)).toContain('zinc');
  });
});

describe('ADR-0034: rate and score formatting', () => {
  it('formatRate handles zero, invalid, sub-1, sub-10 and large rates', () => {
    expect(formatRate(0)).toBe('0/s');
    expect(formatRate(-5)).toBe('0/s');
    expect(formatRate(NaN)).toBe('0/s');
    expect(formatRate(Infinity)).toBe('0/s');
    expect(formatRate(0.05)).toBe('0.05/s');
    expect(formatRate(0.5)).toBe('0.50/s');
    expect(formatRate(0.1)).toBe('0.10/s');
    expect(formatRate(1)).toBe('1.0/s');
    expect(formatRate(3.2)).toBe('3.2/s');
    expect(formatRate(12.4)).toBe('12/s');
  });

  it('formatScore keeps integers compact and formats non-finite as em-dash', () => {
    expect(formatScore(100)).toBe('100');
    expect(formatScore(80)).toBe('80');
    expect(formatScore(0)).toBe('0');
    expect(formatScore(50.5)).toBe('50.5');
    expect(formatScore(NaN)).toBe('—');
  });

  it('isTelemetryFresh enforces the existing 1500 ms staleness convention', () => {
    const now = 1_000_000;
    expect(TELEMETRY_STALE_MS).toBe(1500);
    expect(isTelemetryFresh(0, now)).toBe(false); // never fetched = no data
    expect(isTelemetryFresh(now - 1400, now)).toBe(true);
    expect(isTelemetryFresh(now - 1500, now)).toBe(true); // boundary inclusive
    expect(isTelemetryFresh(now - 1501, now)).toBe(false);
    expect(isTelemetryFresh(now - 999_999, now)).toBe(false); // stale
  });
});

describe('ADR-0034: route-to-device binding', () => {
  const inputUrn = 'alsa://hw:0,0';
  const outputUrn = 'alsa://hw:1,0';

  it('matches when devices.input.urn or devices.output.urn equals the device URN (exact)', () => {
    expect(routeTouchesDevice('room-r1-abc-def', { input: mkDevice({ urn: inputUrn }) }, dev({ urn: inputUrn }))).toBe(true);
    expect(routeTouchesDevice('room-r1-abc-def', { output: mkDevice({ urn: outputUrn }) }, dev({ urn: outputUrn }))).toBe(true);
    // Exact match also works via device id when the daemon keys URN == id
    expect(routeTouchesDevice('room-r1-abc-def', { input: mkDevice({ urn: 'hw:2,0' }) }, dev({ id: 'hw:2,0' }))).toBe(true);
  });

  it('does NOT match a different URN in the devices overlay', () => {
    expect(
      routeTouchesDevice('room-r1-abc-def', { input: mkDevice({ urn: 'alsa://hw:9,9' }) }, dev({ urn: inputUrn, id: 'hw:0,0' }))
    ).toBe(false);
  });

  it('falls back to route-key substring matching (URN / checksum / tail / legacy_id tokens)', () => {
    // Room route IDs embed the endpoint identifiers: room-<roomId>-<inDev>-<outDev>
    expect(routeTouchesDevice('room-live-hw:2,0-hw:1,0', undefined, dev({ id: 'hw:2,0' }))).toBe(true);
    expect(routeTouchesDevice('room-live-0652b7fa-7e5ccbf2', undefined, dev({ checksum: '0652b7fa' }))).toBe(true);
    expect(routeTouchesDevice('room-live-abcdef12-0,0', undefined, dev({ tail: '0,0' }))).toBe(true);
    expect(routeTouchesDevice('room-live-legacy-9-0,0', undefined, dev({ legacyId: 'legacy-9' }))).toBe(true);
    // URN itself embedded verbatim
    expect(routeTouchesDevice('room-live-alsa://hw:0,0-x', undefined, dev({ urn: 'alsa://hw:0,0' }))).toBe(true);
  });

  it('returns false for non-matching routes', () => {
    expect(routeTouchesDevice('room-x-aaaa-bbbb', undefined, dev({ id: 'hw:0,0', urn: 'alsa://hw:0,0' }))).toBe(false);
    expect(routeTouchesDevice('room-x-aaaa-bbbb', {}, dev({ id: 'hw:0,0' }))).toBe(false);
  });

  it('is safe with empty/absent identity tokens (no empty-substring false positives)', () => {
    expect(routeTouchesDevice('room-x-anything', undefined, dev())).toBe(false);
    expect(routeTouchesDevice('room-x-anything', undefined, { id: '', urn: '', tail: '' })).toBe(false);
    expect(routeTouchesDevice('', undefined, dev({ id: 'hw:0,0' }))).toBe(false);
  });
});

describe('ADR-0034: worst peer selection', () => {
  it('returns null when the route has no peers', () => {
    expect(worstPeer(null)).toBeNull();
    expect(worstPeer(undefined)).toBeNull();
    expect(worstPeer(mkHealth({ peers: {} }))).toBeNull();
    expect(worstPeer(mkHealth({ peers: undefined as unknown as Record<string, never> }))).toBeNull();
  });

  it('picks the peer with the highest underruns+overruns+late_packets sum', () => {
    const wp = worstPeer(
      mkHealth({
        peers: {
          'node-a': { underruns: 1, overruns: 0, late_packets: 0 }, // 1
          'node-b': { underruns: 0, overruns: 2, late_packets: 0 }, // 2
          'node-c': { underruns: 0, overruns: 0, late_packets: 5 }, // 5
        },
      })
    );
    expect(wp).not.toBeNull();
    expect(wp!.addr).toBe('node-c');
    expect(wp!.total).toBe(5);
    expect(wp!.late_packets).toBe(5);
  });

  it('breaks ties deterministically by lexicographically smallest address', () => {
    const wp = worstPeer(
      mkHealth({
        peers: {
          'node-z': { underruns: 3, overruns: 0, late_packets: 0 },
          'node-a': { underruns: 0, overruns: 0, late_packets: 3 },
        },
      })
    );
    expect(wp!.addr).toBe('node-a');
    expect(wp!.total).toBe(3);
  });

  it('handles all-zero peers and missing fields without crashing', () => {
    const wp = worstPeer(
      mkHealth({
        peers: {
          'node-b': { underruns: 0, overruns: 0, late_packets: 0 },
          'node-a': { underruns: 0, overruns: 0, late_packets: 0 },
        },
      })
    );
    expect(wp).not.toBeNull();
    expect(wp!.addr).toBe('node-a');
    expect(wp!.total).toBe(0);
  });
});

describe('ADR-0034: tooltip breakdown lines (per-route windowed + distinct shared-device line)', () => {
  it('returns no lines for empty input (no data, no lines)', () => {
    expect(breakdownLines(null, [])).toEqual([]);
    expect(breakdownLines(null, null)).toEqual([]);
    expect(breakdownLines({ urn: '', health: null }, [])).toEqual([]);
  });

  it('emits one sorted windowed line per touching route with scores and e/s rates', () => {
    const lines = breakdownLines(
      null,
      [
        {
          routeId: 'room-b-in-out',
          health: mkHealth({
            score: 80,
            severity: 'MINOR',
            underruns: 1,
            rates: { underrun_per_s: 0.1, overrun_per_s: 0, xrun_per_s: 0, ring_drop_per_s: 0 },
          }),
        },
        {
          routeId: 'room-a-in-out',
          health: mkHealth({
            score: 51,
            severity: 'MAJOR',
            underruns: 5,
            overruns: 2,
            ring_drops: 1,
            rates: { underrun_per_s: 0.5, overrun_per_s: 0.2, xrun_per_s: 0, ring_drop_per_s: 0.1 },
          }),
        },
      ]
    );

    // Deterministic sort by route ID
    expect(lines).toHaveLength(2);
    expect(lines[0]).toBe('room-a-in-out · 51 MAJOR · U 5 (0.50/s) · O 2 (0.20/s) · RD 1 (0.10/s)');
    expect(lines[1]).toBe('room-b-in-out · 80 MINOR · U 1 (0.10/s) · O 0 (0/s) · RD 0 (0/s)');
  });

  it('filters out routes without health (aggregator unwired for that route)', () => {
    const lines = breakdownLines(null, [
      { routeId: 'room-x', health: null },
      { routeId: 'room-y', health: mkHealth({}) },
    ]);
    expect(lines).toHaveLength(1);
    expect(lines[0]).toContain('room-y');
  });

  it('appends a DISTINCT shared-device line (URN, per-direction XRun window/total, severity)', () => {
    const lines = breakdownLines(
      {
        urn: 'alsa://hw:1,0',
        health: mkDevice({
          urn: 'alsa://hw:1,0',
          score: 70,
          severity: 'MINOR',
          output: { xrun_window: 1, xrun_total: 12 },
        }),
      },
      [{ routeId: 'room-a-in-out', health: mkHealth({}) }]
    );

    expect(lines).toHaveLength(2);
    // Device line is last and clearly marked as SHARED (not a route defect)
    expect(lines[1]).toBe('SHARED DEVICE alsa://hw:1,0 · MINOR 70 · out XRun 1/12');
  });

  it('renders both directions when present and the GHOST flag when set', () => {
    const lines = breakdownLines(
      {
        urn: 'alsa://hw:0,0',
        health: mkDevice({
          urn: 'alsa://hw:0,0',
          ghost: true,
          score: 100,
          severity: 'NONE',
          input: { xrun_window: 0, xrun_total: 3 },
          output: { xrun_window: 1, xrun_total: 4 },
        }),
      },
      []
    );
    expect(lines).toHaveLength(1);
    expect(lines[0]).toBe('SHARED DEVICE alsa://hw:0,0 · NONE 100 · GHOST · in XRun 0/3 · out XRun 1/4');
  });

  it('marks "no data" (not clean) when the device entry itself is missing', () => {
    const lines = breakdownLines({ urn: 'alsa://hw:5,5', health: null }, []);
    expect(lines).toEqual(['SHARED DEVICE alsa://hw:5,5 · no data']);
  });

  it('tolerates missing rates (defaults to 0/s) and empty device URN', () => {
    const lines = breakdownLines(null, [
      {
        routeId: 'room-norates',
        health: mkHealth({ score: 80, severity: 'MINOR', rates: undefined as unknown as RouteHealth['rates'] }),
      },
    ]);
    expect(lines[0]).toBe('room-norates · 80 MINOR · U 0 (0/s) · O 0 (0/s) · RD 0 (0/s)');
  });
});
