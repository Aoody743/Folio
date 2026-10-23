import { Icon as AstryxIcon } from "@astryxdesign/core/Icon";
import {
  LayoutGrid, Globe, Layers, Folder, Ellipsis, Search, Plus, ChevronRight,
  X, StickyNote, ClipboardCheck, Bell, Settings2, ShieldCheck, SquareTerminal,
  History, Check, Pin, RefreshCw, LogOut, Server, Cpu, MemoryStick, HardDrive,
  Database, Boxes, Package, AppWindow, ArrowLeft, ArrowRight, Maximize,
  Minimize, Keyboard, Bookmark, ExternalLink, Download, Upload, SlidersHorizontal,
  FileText, Archive, KeyRound, Clock, CircleHelp, Monitor, Circle, Trash2,
  Square, Network, CircleAlert, LockKeyhole, Code2, GitBranch,
} from "lucide-react";
const icons = {
  overview: LayoutGrid, sites: Globe, software: Layers, files: Folder,
  more: Ellipsis, search: Search, plus: Plus, arrow: ChevronRight, close: X,
  note: StickyNote, tasks: ClipboardCheck, events: Bell, settings: Settings2,
  shield: ShieldCheck, terminal: SquareTerminal, history: History, check: Check,
  pin: Pin, refresh: RefreshCw, logout: LogOut, server: Server, cpu: Cpu,
  memory: MemoryStick, disk: HardDrive, database: Database, cache: Boxes,
  package: Package, browser: AppWindow, back: ArrowLeft, forward: ArrowRight,
  expand: Maximize, collapse: Minimize, keyboard: Keyboard, bookmark: Bookmark,
  external: ExternalLink, download: Download, upload: Upload, filter: SlidersHorizontal,
  logs: FileText, backup: Archive, key: KeyRound, clock: Clock, help: CircleHelp,
  monitor: Monitor, dot: Circle, trash: Trash2, stop: Square, network: Network,
  warning: CircleAlert, lock: LockKeyhole, code: Code2, git: GitBranch,
} as const;
export type IconName = keyof typeof icons;
export function Icon({ name, size = 20 }: { name: IconName; size?: number }) {
  return <AstryxIcon icon={icons[name]} color="inherit" style={{ width: size, height: size }} strokeWidth={1.75} />;
}
