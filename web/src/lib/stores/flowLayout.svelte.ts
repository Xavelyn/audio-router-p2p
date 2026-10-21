/**
 * Audio Router P2P - Audio Flow Canvas Layout Store (ADR 0012)
 *
 * Manages client presentation state for SvelteFlow audio routing canvas:
 * - Selective device staging (enabledDevices)
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
  // Staged device IDs visible on the canvas
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

  private saveTimeout: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    if (typeof window !== 'undefined') {
      this.hydrate();
    }
  }

  /**
   * Hydrate layout state from localStorage with schema versioning and corruption recovery
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
        return;
      }

      const parsed = JSON.parse(raw) as FlowLayoutStorageV1;

      // Schema compatibility verification
      if (!parsed || parsed.version !== 1) {
        console.warn(`[flowLayoutStore] Incompatible layout schema version (expected 1, got ${parsed?.version}). Resetting to default.`);
        this.isHydrated = true;
        return;
      }

      if (parsed.sidebarCollapsed !== undefined) {
        this.sidebarOpen = !parsed.sidebarCollapsed;
      }

      if (Array.isArray(parsed.stagedDeviceIds)) {
        this.enabledDevices = new Set(parsed.stagedDeviceIds);
      }

      if (Array.isArray(parsed.collapsedNodes)) {
        this.collapsedNodes = new Set(parsed.collapsedNodes);
      }

      if (parsed.nodePositions && typeof parsed.nodePositions === 'object') {
        const nextPos = new Map<string, { x: number; y: number }>();
        for (const [key, val] of Object.entries(parsed.nodePositions)) {
          if (val && typeof val.x === 'number' && typeof val.y === 'number' && !isNaN(val.x) && !isNaN(val.y)) {
            nextPos.set(key, { x: Math.round(val.x), y: Math.round(val.y) });
          }
        }
        this.positions = nextPos;
      }

      if (parsed.viewport && typeof parsed.viewport.zoom === 'number') {
        this.viewport = parsed.viewport;
      }

      this.isHydrated = true;
    } catch (err) {
      console.warn('[flowLayoutStore] Failed to parse stored canvas layout; recovering with clean defaults:', err);
      this.isHydrated = true;
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
      } catch (err) {
        console.warn('[flowLayoutStore] Failed to persist audio flow canvas layout to localStorage:', err);
      }
    }, 300);
  }

  /**
   * Check if device is currently staged on canvas
   */
  isDeviceEnabled(deviceId: string): boolean {
    return this.enabledDevices.has(deviceId);
  }

  /**
   * Stage a device on the canvas, optionally setting its initial position
   */
  enableDevice(deviceId: string, pos?: { x: number; y: number }, targetNodeId?: string) {
    const next = new Set(this.enabledDevices);
    next.add(deviceId);
    this.enabledDevices = next;

    if (pos) {
      const key = targetNodeId || deviceId;
      const nextPositions = new Map(this.positions);
      nextPositions.set(key, { x: Math.round(pos.x), y: Math.round(pos.y) });
      this.positions = nextPositions;
    }

    this.queuePersist();
  }

  /**
   * Remove a device from the canvas staging and clean up associated positions
   */
  disableDevice(deviceId: string) {
    const next = new Set(this.enabledDevices);
    next.delete(deviceId);
    this.enabledDevices = next;

    const nextPositions = new Map(this.positions);
    nextPositions.delete(deviceId);
    nextPositions.delete(`input-${deviceId}`);
    nextPositions.delete(`output-${deviceId}`);
    this.positions = nextPositions;

    this.queuePersist();
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
    this.queuePersist();
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
    if (this.enabledDevices.size === 0 && defaultDeviceIds.length > 0) {
      this.enabledDevices = new Set(defaultDeviceIds);
      this.queuePersist();
    }
  }

  /**
   * Reset layout entirely to defaults
   */
  resetLayout(defaultDeviceIds?: string[]) {
    this.positions = new Map();
    this.enabledDevices = new Set(defaultDeviceIds || []);
    this.queuePersist();
  }
}

export const flowLayoutStore = new FlowLayoutStore();
