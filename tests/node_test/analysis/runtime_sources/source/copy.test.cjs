const value = require("../value.cjs");
if (value !== 42) throw new Error("staged dependency lost its identity");
console.log("local runtime copy executed");
