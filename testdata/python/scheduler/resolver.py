"""Dependency resolver - topological sort with priority ordering."""


def resolve_order(tasks):
    """Sort tasks respecting dependencies, then by priority (higher = run first)."""
    task_map = {t.name: t for t in tasks}
    resolved = []
    seen = set()

    def visit(task):
        if task.name in seen:
            return
        seen.add(task.name)
        for dep_name in task.depends_on:
            dep = task_map[dep_name]
            visit(dep)
        resolved.append(task)

    # BUG: sorts ascending — lower priority tasks get visited first
    # Fix: add reverse=True
    for t in sorted(tasks, key=lambda t: t.priority):
        visit(t)

    return resolved
