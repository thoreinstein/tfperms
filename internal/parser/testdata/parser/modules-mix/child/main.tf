# Placeholder so ./child resolves to a directory with at least one .tf
# file. This scenario exercises Parse's classification of module sources;
# the recursive expansion of "./child" must succeed and contribute zero
# resources rather than failing the walk with a "no .tf files" error.
