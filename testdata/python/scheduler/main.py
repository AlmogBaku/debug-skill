"""Task scheduler - demonstrates a priority ordering bug."""
from models import Task
from runner import run_tasks


def main():
    tasks = [
        Task("deploy", priority=10, depends_on=["build", "test"]),
        Task("build", priority=8),
        Task("test", priority=9, depends_on=["build"]),
        Task("lint", priority=2),
        Task("docs", priority=1),
    ]

    print("=== Task Scheduler ===")
    print(f"Tasks: {tasks}")
    print()
    print("Execution order:")
    order = run_tasks(tasks)
    print()

    # Higher priority should run first (after satisfying deps)
    expected = ["build", "test", "deploy", "lint", "docs"]
    if order != expected:
        print(f"BUG! Expected {expected}")
        print(f"     Got      {order}")
    else:
        print("All tasks executed in correct order!")


if __name__ == "__main__":
    main()
