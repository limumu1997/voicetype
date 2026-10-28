"""Classify the focused widget via AT-SPI; prints one of:
editable | terminal | none | unknown

Run by the daemon with the system python3 (needs python3-gi + gir1.2-atspi-2.0).
Only windows in the ACTIVE state are searched. Apps that expose no accessibility
tree while a11y is off (Chrome, Electron) leave a stale ACTIVE window with no
focused descendant, so they fall through to "unknown".
"""
import sys

try:
    import gi

    gi.require_version("Atspi", "2.0")
    from gi.repository import Atspi
except Exception:
    print("unknown")
    sys.exit(0)

S = Atspi.StateType
budget = [5000]  # node cap so a huge tree can't stall the paste


def find_focused(node, depth):
    budget[0] -= 1
    if budget[0] < 0 or depth > 60:
        return None
    try:
        states = node.get_state_set()
        if states.contains(S.FOCUSED):
            return node
        if depth > 1 and not states.contains(S.SHOWING):
            return None  # the focus can't be inside a hidden subtree
        for i in range(node.get_child_count()):
            child = node.get_child_at_index(i)
            if child is not None:
                hit = find_focused(child, depth + 1)
                if hit is not None:
                    return hit
    except Exception:
        pass
    return None


def classify():
    desktop = Atspi.get_desktop(0)
    for i in range(desktop.get_child_count()):
        app = desktop.get_child_at_index(i)
        if app is None:
            continue
        try:
            for j in range(app.get_child_count()):
                win = app.get_child_at_index(j)
                if win is None or not win.get_state_set().contains(S.ACTIVE):
                    continue
                hit = find_focused(win, 0)
                if hit is None:
                    continue
                if hit.get_role() == Atspi.Role.TERMINAL:
                    return "terminal"
                if hit.get_state_set().contains(S.EDITABLE):
                    return "editable"
                return "none"
        except Exception:
            continue
    return "unknown"


print(classify())
