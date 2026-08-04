export type SaveResponseAction = 'load' | 'preserve' | 'preserve-and-sync' | 'resync';

export function saveResponseAction(
  stateAccepted: boolean,
  sameNote: boolean,
  postSubmitChanges: boolean,
): SaveResponseAction {
  if (!sameNote) return 'resync';
  if (!stateAccepted) return 'preserve-and-sync';
  return postSubmitChanges ? 'preserve' : 'load';
}

export function hasPostSubmitChanges(
  currentBody: string,
  currentTags: readonly string[],
  submittedBody: string,
  submittedTags: readonly string[],
): boolean {
  return currentBody !== submittedBody || !sameStrings(currentTags, submittedTags);
}

function sameStrings(first: readonly string[], second: readonly string[]): boolean {
  return first.length === second.length && first.every((value, index) => value === second[index]);
}
