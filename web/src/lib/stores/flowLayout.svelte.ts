/**
 * Audio Router P2P - Audio Flow Canvas Layout Store (ADR 0012, ADR 0027)
 *
 * Manages client presentation state for SvelteFlow audio routing canvas:
 * - Directional endpoint staging (in:<key>, out:<key>) with ADR 0027 isolation
 * - Backward compatibility with V1 schema (audio_router_flow_layout_v1)
 * - Persistent (x, y) coordinates for input, room, and output nodes
 * - Palette sidebar collapsed/open state
 * - Collapsed cluster node accordions
 * - Trailing-edge 300ms debounced persistence to localStorage (audio_router_flow_layout_v1)
 * - Safe schema validation and corruption recovery
 */

export const STORAGE_KEY_V1 = 'audio_router_flow_layout_v1';

export interface FlowLayoutStorageV1 {
  version: 1;
  lastUpdated: number;
  sidebarCollapsed: boolean;
  stagedDeviceIds: string[];
  nodePositions: Record<string, { x: number; y: number }>;
  viewport?: {
    x: number;
    y: number;
    zoom: number;
  };
  collapsedNodes?: string[];
}

export class FlowLayoutStore {
  // Staged device / directional endpoint IDs visible on the canvas (e.g. "in:hw:0", "out:hw:0")
  enabledDevices = $state<Set<string>>(new Set());

  // Spatial coordinates keyed by SvelteFlow node ID (e.g. "input-hw:0", "room-main", "output-hw:1")
  positions = $state<Map<string, { x: number; y: number }>>(new Map());

  // Palette drawer visibility
  sidebarOpen = $state<boolean>(true);

  // Set of cluster node IDs whose accordions in the palette are collapsed
  collapsedNodes = $state<Set<string>>(new Set());

  // Viewport pan & zoom
  viewport = $state<{ x: number; y: number; zoom: number } | null>(null);

  // Hydration readiness flag
  isHydrated = $state<boolean>(false);

  // Flag indicating whether a saved layout was found in localStorage
  hasSavedLayout = $state<boolean>(false);

  private saveTimeout: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    if (typeof window !== 'undefined') {
      this.hydrate();
      window.addEventListener('beforeunload', () => {
        this.persistNow();
      });
    }
  }

  /**
   * Hydrate layout state from localStorage with schema versioning,
   * ADR 0027 directional normalization, and corruption recovery.
   */
  hydrate() {
    if (typeof window === 'undefined' || typeof localStorage === 'undefined') {
      this.isHydrated = true;
      return;
    }

    try {
      const raw = localStorage.getItem(STORAGE_KEY_V1);
      if (!raw) {
        this.isHydrated = true;
        this.hasSavedLayout = false;
        return;
      }

      const parsed = JSON.parse(raw) as FlowLayoutStorageV1;

      // Schema compatibility verification
      if (!parsed || parsed.version !== 1) {
        console.warn(`[flowLayoutStore] Incompatible layout schema version (expected 1, got ${parsed?.version}). Resetting to default.`);
        this.isHydrated = true;
        this.hasSavedLayout = false;
        return;
      }

      this.hasSavedLayout = true;

      if (parsed.sidebarCollapsed !== undefined) {
        this.sidebarOpen = !parsed.sidebarCollapsed;
      }

      const positionsMap = new Map<string, { x: number; y: number }>();
      const positionsObj = parsed.nodePositions && typeof parsed.nodePositions === 'object' ? parsed.nodePositions : {};

      if (parsed.nodePositions && typeof parsed.nodePositions === 'object') {
        for (const [key, val] of Object.entries(parsed.nodePositions)) {
          if (val && typeof val.x === 'number' && typeof val.y === 'number' && !isNaN(val.x) && !isNaN(val.y)) {
            positionsMap.set(key, { x: Math.round(val.x), y: Math.round(val.y) });
          }
        }
        this.positions = positionsMap;
      }

      // ADR 0027: Directional normalization for stagedDeviceIds
      if (Array.isArray(parsed.stagedDeviceIds)) {
        const nextEnabled = new Set<string>();

        for (const rawId of parsed.stagedDeviceIds) {
          if (typeof rawId !== 'string' || !rawId.trim()) continue;

          // Already directional tag: "in:..." or "out:..."
          if (rawId.startsWith('in:') || rawId.startsWith('out:')) {
            nextEnabled.add(rawId);
            continue;
          }

          // Legacy SvelteFlow node IDs
          if (rawId.startsWith('input-')) {
            nextEnabled.add(`in:${rawId.slice(6)}`);
            continue;
          }
          if (rawId.startsWith('output-')) {
            nextEnabled.add(`out:${rawId.slice(7)}`);
            continue;
          }

          // Legacy bare device keys (e.g. "hw:0" or "node2/hw:1")
          // Inspect positions to infer whether input, output, or duplex was staged
          const hasInPos = positionsObj[`input-${rawId}`] !== undefined;
          const hasOutPos = positionsObj[`output-${rawId}`] !== undefined;

          if (hasInPos && !hasOutPos) {
            nextEnabled.add(`in:${rawId}`);
          } else if (hasOutPos && !hasInPos) {
            nextEnabled.add(`out:${rawId}`);
          } else {
            // Both directional positions exist or neither exists (default duplex)
            nextEnabled.add(`in:${rawId}`);
            nextEnabled.add(`out:${rawId}`);
          }
        }

        this.enabledDevices = nextEnabled;
      }

      if (Array.isArray(parsed.collapsedNodes)) {
        this.collapsedNodes = new Set(parsed.collapsedNodes);
      }

      if (parsed.viewport && typeof parsed.viewport.zoom === 'number') {
        this.viewport = parsed.viewport;
      }

      this.isHydrated = true;
    } catch (err) {
      console.warn('[flowLayoutStore] Failed to parse stored canvas layout; recovering with clean defaults:', err);
      this.isHydrated = true;
      this.hasSavedLayout = false;
    }
  }

  /**
   * Persist immediately to localStorage (synchronous write)
   */
  persistNow() {
    if (typeof window === 'undefined' || typeof localStorage === 'undefined' || !this.isHydrated) {
      return;
    }

    if (this.saveTimeout) {
      clearTimeout(this.saveTimeout);
      this.saveTimeout = null;
    }

    try {
      const nodePositionsRecord: Record<string, { x: number; y: number }> = {};
      for (const [nodeId, pos] of this.positions.entries()) {
        nodePositionsRecord[nodeId] = { x: Math.round(pos.x), y: Math.round(pos.y) };
      }

      const payload: FlowLayoutStorageV1 = {
        version: 1,
        lastUpdated: Date.now(),
        sidebarCollapsed: !this.sidebarOpen,
        stagedDeviceIds: Array.from(this.enabledDevices),
        nodePositions: nodePositionsRecord,
        collapsedNodes: Array.from(this.collapsedNodes),
        viewport: this.viewport ?? undefined,
      };

      localStorage.setItem(STORAGE_KEY_V1, JSON.stringify(payload));
      this.hasSavedLayout = true;
    } catch (err) {
      console.warn('[flowLayoutStore] Failed to persist audio flow canvas layout to localStorage:', err);
    }
  }

  /**
   * Queue debounced trailing write to localStorage (300ms delay)
   */
  private queuePersist() {
    if (typeof window === 'undefined' || typeof localStorage === 'undefined' || !this.isHydrated) {
      return;
    }

    if (this.saveTimeout) {
      clearTimeout(this.saveTimeout);
    }

    this.saveTimeout = setTimeout(() => {
      this.persistNow();
    }, 300);
  }

  // =========================================================================
  // ADR 0027 Directional Staging Primitives
  // =========================================================================

  /**
   * Check if a specific directional endpoint is staged on canvas.
   * Matches "in:<canvasKey>", "out:<canvasKey>", SvelteFlow node IDs ("input-...", "output-..."),
   * and legacy bare keys.
   */
  isEndpointStaged(canvasKey: string, direction: 'input' | 'output'): boolean {
    const dirTag = direction === 'input' ? 'in:' : 'out:';
    const nodePrefix = direction === 'input' ? 'input-' : 'output-';
    const cleanKey = canvasKey.includes('/') ? canvasKey.slice(canvasKey.indexOf('/') + 1) : canvasKey;

    // 1. Direct key lookups
    if (this.enabledDevices.has(`${dirTag}${canvasKey}`)) return true;
    if (this.enabledDevices.has(`${nodePrefix}${canvasKey}`)) return true;
    if (this.enabledDevices.has(`${dirTag}${cleanKey}`)) return true;
    if (this.enabledDevices.has(`${nodePrefix}${cleanKey}`)) return true;

    // 2. Scan enabledDevices for prefix/suffix and legacy bare matches
    for (const d of this.enabledDevices) {
      // Directional tag match
      if (d.startsWith(dirTag) || d.startsWith(nodePrefix)) {
        const stripped = d.startsWith(dirTag) ? d.slice(dirTag.length) : d.slice(nodePrefix.length);
        if (
          stripped === canvasKey ||
          stripped === cleanKey ||
          stripped.endsWith(`/${cleanKey}`) ||
          cleanKey.endsWith(`/${stripped}`)
        ) {
          return true;
        }
      }

      // Legacy un-prefixed bare device key fallback
      if (
        !d.startsWith('in:') &&
        !d.startsWith('out:') &&
        !d.startsWith('input-') &&
        !d.startsWith('output-')
      ) {
        if (
          d === canvasKey ||
          d === cleanKey ||
          d.endsWith(`/${cleanKey}`) ||
          cleanKey.endsWith(`/${d}`)
        ) {
          return true;
        }
      }
    }

    return false;
  }

  /**
   * Stage only the specified directional endpoint by adding "in:<canvasKey>"
   * or "out:<canvasKey>" to enabledDevices, and optionally storing its position.
   */
  stageEndpoint(canvasKey: string, direction: 'input' | 'output', pos?: { x: number; y: number }) {
    const dirTag = `${direction === 'input' ? 'in:' : 'out:'}${canvasKey}`;
    const next = new Set(this.enabledDevices);
    next.add(dirTag);
    this.enabledDevices = next;

    if (pos) {
      const nodeKey = `${direction === 'input' ? 'input-' : 'output-'}${canvasKey}`;
      const nextPositions = new Map(this.positions);
      nextPositions.set(nodeKey, { x: Math.round(pos.x), y: Math.round(pos.y) });
      this.positions = nextPositions;
    }

    this.persistNow();
  }

  /**
   * Unstage only the specified directional endpoint.
   * Removes "in:<canvasKey>" or "out:<canvasKey>", deletes its directional position,
   * and preserves the counterpart directional endpoint (migrating legacy bare keys if present).
   */
  unstageEndpoint(canvasKey: string, direction: 'input' | 'output') {
    const cleanKey = canvasKey.includes('/') ? canvasKey.slice(canvasKey.indexOf('/') + 1) : canvasKey;
    const dirTag = direction === 'input' ? 'in:' : 'out:';
    const nodePrefix = direction === 'input' ? 'input-' : 'output-';
    const counterpartDirection = direction === 'input' ? 'output' : 'input';
    const counterpartTag = `${counterpartDirection === 'input' ? 'in:' : 'out:'}${canvasKey}`;

    const next = new Set(this.enabledDevices);

    // 1. Detect and migrate legacy bare keys to preserve the un-dismissed counterpart
    let hadBareKey = false;
    for (const d of next) {
      if (
        !d.startsWith('in:') &&
        !d.startsWith('out:') &&
        !d.startsWith('input-') &&
        !d.startsWith('output-')
      ) {
        if (d === canvasKey || d === cleanKey || d.endsWith(`/${cleanKey}`) || cleanKey.endsWith(`/${d}`)) {
          hadBareKey = true;
          next.delete(d);
        }
      }
    }

    if (hadBareKey) {
      next.add(counterpartTag);
    }

    // 2. Remove all directional entries matching this direction
    for (const d of Array.from(next)) {
      if (d.startsWith(dirTag) || d.startsWith(nodePrefix)) {
        const stripped = d.startsWith(dirTag) ? d.slice(dirTag.length) : d.slice(nodePrefix.length);
        if (
          stripped === canvasKey ||
          stripped === cleanKey ||
          stripped.endsWith(`/${cleanKey}`) ||
          cleanKey.endsWith(`/${stripped}`)
        ) {
          next.delete(d);
        }
      }
    }

    this.enabledDevices = next;

    // 3. Remove ONLY positions for this direction, preserving counterpart positions
    const nextPositions = new Map(this.positions);
    nextPositions.delete(`${nodePrefix}${canvasKey}`);
    nextPositions.delete(`${nodePrefix}${cleanKey}`);
    for (const k of Array.from(nextPositions.keys())) {
      if (k.startsWith(nodePrefix)) {
        const stripped = k.slice(nodePrefix.length);
        if (
          stripped === canvasKey ||
          stripped === cleanKey ||
          stripped.endsWith(`/${cleanKey}`) ||
          cleanKey.endsWith(`/${stripped}`)
        ) {
          nextPositions.delete(k);
        }
      }
    }
    this.positions = nextPositions;

    this.persistNow();
  }

  // =========================================================================
  // Backward-Compatibility Methods (ADR 0012 / ADR 0024 Callers)
  // =========================================================================

  /**
   * Check if device is currently staged on canvas (either input or output endpoint).
   */
  isDeviceEnabled(deviceId: string, nodeId?: string, direction?: 'input' | 'output'): boolean {
    const canvasKey = nodeId && !deviceId.includes('/') ? `${nodeId}/${deviceId}` : deviceId;
    if (direction) {
      return this.isEndpointStaged(canvasKey, direction);
    }
    return this.isEndpointStaged(canvasKey, 'input') || this.isEndpointStaged(canvasKey, 'output');
  }

  /**
   * Stage a device on the canvas, optionally setting its initial position.
   * If direction is provided or inferrable from targetNodeId, stages that endpoint specifically;
   * otherwise stages both input and output endpoints for duplex compatibility.
   */
  enableDevice(
    deviceId: string,
    pos?: { x: number; y: number },
    targetNodeId?: string,
    direction?: 'input' | 'output'
  ) {
    if (direction) {
      this.stageEndpoint(deviceId, direction, pos);
      return;
    }

    if (targetNodeId?.startsWith('input-') || deviceId.startsWith('input-') || deviceId.startsWith('in:')) {
      const clean = deviceId.replace(/^(input-|in:)/, '');
      this.stageEndpoint(clean, 'input', pos);
      if (pos && targetNodeId) {
        this.positions.set(targetNodeId, { x: Math.round(pos.x), y: Math.round(pos.y) });
      }
      return;
    }

    if (targetNodeId?.startsWith('output-') || deviceId.startsWith('output-') || deviceId.startsWith('out:')) {
      const clean = deviceId.replace(/^(output-|out:)/, '');
      this.stageEndpoint(clean, 'output', pos);
      if (pos && targetNodeId) {
        this.positions.set(targetNodeId, { x: Math.round(pos.x), y: Math.round(pos.y) });
      }
      return;
    }

    // Unspecified direction: stage both endpoints and store position
    const next = new Set(this.enabledDevices);
    next.add(`in:${deviceId}`);
    next.add(`out:${deviceId}`);
    this.enabledDevices = next;

    if (pos) {
      const key = targetNodeId || deviceId;
      const nextPositions = new Map(this.positions);
      nextPositions.set(key, { x: Math.round(pos.x), y: Math.round(pos.y) });
      nextPositions.set(`input-${deviceId}`, { x: Math.round(pos.x), y: Math.round(pos.y) });
      nextPositions.set(`output-${deviceId}`, { x: Math.round(pos.x), y: Math.round(pos.y) });
      this.positions = nextPositions;
    }

    this.persistNow();
  }

  /**
   * Remove a device from canvas staging and clean up associated positions.
   * If direction is passed, delegates to unstageEndpoint; otherwise removes all directional endpoints.
   */
  disableDevice(deviceId: string, nodeId?: string, direction?: 'input' | 'output') {
    const canvasKey = nodeId && !deviceId.includes('/') ? `${nodeId}/${deviceId}` : deviceId;
    if (direction) {
      this.unstageEndpoint(canvasKey, direction);
      return;
    }

    const cleanKey = deviceId.includes('/') ? deviceId.slice(deviceId.indexOf('/') + 1) : deviceId;

    const next = new Set(this.enabledDevices);
    for (const d of Array.from(next)) {
      const stripped = d.replace(/^(in:|out:|input-|output-)/, '');
      if (
        d === deviceId ||
        d === canvasKey ||
        d === cleanKey ||
        stripped === deviceId ||
        stripped === canvasKey ||
        stripped === cleanKey ||
        stripped.endsWith(`/${cleanKey}`) ||
        cleanKey.endsWith(`/${stripped}`)
      ) {
        next.delete(d);
      }
    }
    this.enabledDevices = next;

    const nextPositions = new Map(this.positions);
    nextPositions.delete(deviceId);
    nextPositions.delete(canvasKey);
    nextPositions.delete(cleanKey);
    nextPositions.delete(`input-${deviceId}`);
    nextPositions.delete(`output-${deviceId}`);
    nextPositions.delete(`input-${canvasKey}`);
    nextPositions.delete(`output-${canvasKey}`);
    if (nodeId) {
      nextPositions.delete(`${nodeId}/${deviceId}`);
      nextPositions.delete(`input-${nodeId}/${deviceId}`);
      nextPositions.delete(`output-${nodeId}/${deviceId}`);
    }
    for (const k of Array.from(nextPositions.keys())) {
      const stripped = k.replace(/^(input-|output-)/, '');
      if (
        stripped === deviceId ||
        stripped === canvasKey ||
        stripped === cleanKey ||
        stripped.endsWith(`/${cleanKey}`)
      ) {
        nextPositions.delete(k);
      }
    }
    this.positions = nextPositions;

    this.persistNow();
  }

  /**
   * Toggle staging of a device
   */
  toggleDevice(deviceId: string, pos?: { x: number; y: number }) {
    if (this.isDeviceEnabled(deviceId)) {
      this.disableDevice(deviceId);
    } else {
      this.enableDevice(deviceId, pos);
    }
  }

  /**
   * Store coordinate for a node
   */
  setPosition(nodeId: string, pos: { x: number; y: number }) {
    const next = new Map(this.positions);
    next.set(nodeId, { x: Math.round(pos.x), y: Math.round(pos.y) });
    this.positions = next;
    this.queuePersist();
  }

  /**
   * Remove stored coordinate for a specific node ID (e.g. deleted room)
   */
  deleteNodePosition(nodeId: string) {
    if (this.positions.has(nodeId)) {
      const next = new Map(this.positions);
      next.delete(nodeId);
      this.positions = next;
      this.persistNow();
    }
  }

  /**
   * Retrieve coordinate for a node
   */
  getPosition(nodeId: string): { x: number; y: number } | undefined {
    return this.positions.get(nodeId);
  }

  /**
   * Clear all stored node positions (for auto-arrange recomputation)
   */
  clearPositions() {
    this.positions = new Map();
    this.persistNow();
  }

  /**
   * Toggle sidebar drawer visibility
   */
  toggleSidebar() {
    this.sidebarOpen = !this.sidebarOpen;
    this.queuePersist();
  }

  /**
   * Set sidebar drawer visibility explicitly
   */
  setSidebarOpen(open: boolean) {
    this.sidebarOpen = open;
    this.queuePersist();
  }

  /**
   * Check if a cluster node accordion in the palette is collapsed
   */
  isNodeCollapsed(nodeId: string): boolean {
    return this.collapsedNodes.has(nodeId);
  }

  /**
   * Toggle accordion collapse state for a cluster node in the palette
   */
  toggleNodeCollapsed(nodeId: string) {
    const next = new Set(this.collapsedNodes);
    if (next.has(nodeId)) {
      next.delete(nodeId);
    } else {
      next.add(nodeId);
    }
    this.collapsedNodes = next;
    this.queuePersist();
  }

  /**
   * Set accordion collapse state for a cluster node explicitly
   */
  setNodeCollapsed(nodeId: string, collapsed: boolean) {
    const next = new Set(this.collapsedNodes);
    if (collapsed) {
      next.add(nodeId);
    } else {
      next.delete(nodeId);
    }
    this.collapsedNodes = next;
    this.queuePersist();
  }

  /**
   * Save viewport pan/zoom
   */
  setViewport(vp: { x: number; y: number; zoom: number }) {
    this.viewport = vp;
    this.queuePersist();
  }

  /**
   * Initialize default staged devices on first load if none are saved
   */
  initDefaultsIfEmpty(defaultDeviceIds: string[]) {
    if (!this.hasSavedLayout && this.enabledDevices.size === 0 && defaultDeviceIds.length > 0) {
      const set = new Set<string>();
      for (const id of defaultDeviceIds) {
        if (id.startsWith('in:') || id.startsWith('out:')) {
          set.add(id);
        } else {
          set.add(`in:${id}`);
          set.add(`out:${id}`);
        }
      }
      this.enabledDevices = set;
      this.persistNow();
    }
  }

  /**
   * Reset layout entirely to defaults
   */
  resetLayout(defaultDeviceIds?: string[]) {
    this.positions = new Map();
    const set = new Set<string>();
    if (defaultDeviceIds) {
      for (const id of defaultDeviceIds) {
        if (id.startsWith('in:') || id.startsWith('out:')) {
          set.add(id);
        } else {
          set.add(`in:${id}`);
          set.add(`out:${id}`);
        }
      }
    }
    this.enabledDevices = set;
    this.persistNow();
  }
}

export const flowLayoutStore = new FlowLayoutStore();
