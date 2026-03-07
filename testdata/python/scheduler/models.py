"""Task model definitions."""


class Task:
    def __init__(self, name, priority, depends_on=None):
        self.name = name
        self.priority = priority
        self.depends_on = depends_on or []
        self.completed = False

    def __repr__(self):
        return f"Task({self.name}, pri={self.priority})"
