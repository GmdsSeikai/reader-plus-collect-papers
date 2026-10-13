import releaseData from "./releases.json";
import type { GuidePreferences } from "@reader/core";

export interface ReleaseItem {
  title: string;
  body?: string;
  /** A guide topic that demonstrates this change. */
  topic?: string;
}

/**
 * Notes for important releases, keyed by the desktop package version. A
 * version without an entry updates silently.
 */
export const releaseNotes: Record<string, ReleaseItem[]> = releaseData;

export type GuideLaunch =
  | { kind: "onboarding" }
  | { kind: "release"; items: ReleaseItem[] }
  | { kind: "none"; save?: GuidePreferences };

/**
 * Decides what to show at startup. Libraries created before the guide existed
 * count as onboarded, so existing users see release notes instead.
 */
export function guideLaunch(
  preferences: GuidePreferences,
  version: string,
  hasDocuments: boolean,
): GuideLaunch {
  if (!preferences.onboarded && !hasDocuments) return { kind: "onboarding" };
  if (preferences.seenVersion === version)
    return preferences.onboarded
      ? { kind: "none" }
      : { kind: "none", save: { ...preferences, onboarded: true } };
  const items = releaseNotes[version];
  if (items?.length) return { kind: "release", items };
  return {
    kind: "none",
    save: { ...preferences, onboarded: true, seenVersion: version },
  };
}
