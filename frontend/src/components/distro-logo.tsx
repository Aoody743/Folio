import { Icon } from "./icon";
export function distroLogo(os?: string): string | undefined {
  const name = (os || "").toLowerCase();
  const matches: [RegExp, string][] = [
    [/ubuntu/, "ubuntu"], [/debian/, "debian"], [/fedora/, "fedora"], [/arch/, "archlinux"],
    [/alpine/, "alpinelinux"], [/rocky/, "rockylinux"], [/alma/, "almalinux"], [/suse/, "opensuse"],
    [/centos/, "centos"], [/red\s*hat|rhel/, "redhat"], [/mint/, "linuxmint"], [/linux/, "linux"],
  ];
  return matches.find(([pattern]) => pattern.test(name))?.[1];
}
export function DistroLogo({os}: {os?: string}) {
  const logo = distroLogo(os);
  return logo ? <span className="distro-logo" role="img" aria-label={os} title={os} style={{maskImage:`url(/distro-logos/${logo}.svg)`,WebkitMaskImage:`url(/distro-logos/${logo}.svg)`}}/> : <span className="distro-logo-fallback"><Icon name="server" size={18}/></span>;
}
