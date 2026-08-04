export const appearanceModes = ['system', 'light', 'dark'] as const;
export const builtInThemes = ['remember', 'nord', 'dracula', 'solarized', 'catppuccin'] as const;

export type AppearanceMode = typeof appearanceModes[number];
export type Theme = typeof builtInThemes[number];
export type Scheme = 'light' | 'dark';

export function storedMode(value: string | null): AppearanceMode {
  return appearanceModes.includes(value as AppearanceMode) ? value as AppearanceMode : 'system';
}

export function storedTheme(value: string | null): Theme {
  return builtInThemes.includes(value as Theme) ? value as Theme : 'remember';
}

export function resolvedScheme(mode: AppearanceMode, systemDark: boolean): Scheme {
  return mode === 'system' ? (systemDark ? 'dark' : 'light') : mode;
}
