"""Task runner - executes tasks in resolved order."""
from resolver import resolve_order


def run_tasks(tasks):
    """Execute tasks in dependency-respecting priority order."""
    order = resolve_order(tasks)
    results = []
    for task in order:
        print(f"  Running: {task.name} (priority={task.priority})")
        task.completed = True
        results.append(task.name)
    return results
