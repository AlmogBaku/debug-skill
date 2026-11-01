interface Person {
  name: string;
  age: number;
}

function greet(p: Person): string {
  const label: string = p.age >= 18 ? "adult" : "minor";
  return `Hello ${p.name} (${label})`;
}

const message: string = greet({ name: "Ada", age: 36 });
console.log(message);
