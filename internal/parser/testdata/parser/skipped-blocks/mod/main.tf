# Placeholder so ./mod resolves to a directory with at least one .tf
# file. This scenario asserts the silent-skip behaviour of non-resource
# top-level blocks; the recursive expansion of "./mod" must succeed and
# contribute zero resources rather than emitting a "could not load"
# warning that would obscure the skip-coverage assertion.
