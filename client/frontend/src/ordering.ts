export type Version = { generation: number; revision: number };

export function acceptsVersion(active: Version, candidate: Version): boolean {
  return candidate.generation > active.generation ||
    (candidate.generation === active.generation && candidate.revision >= active.revision);
}
