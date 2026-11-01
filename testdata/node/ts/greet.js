function greet(p) {
  const label = p.age >= 18 ? "adult" : "minor";
  return `Hello ${p.name} (${label})`;
}
const message = greet({ name: "Ada", age: 36 });
console.log(message);
//# sourceMappingURL=greet.js.map
